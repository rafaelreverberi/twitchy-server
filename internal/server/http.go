package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,199}$`)
var loginPattern = regexp.MustCompile(`^[a-zA-Z0-9_]{1,25}$`)
var initPattern = regexp.MustCompile(`^init[0-9]{9,15}\.mp4$`)
var vodIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
var segmentPattern = regexp.MustCompile(`^seg[0-9]{9,15}\.ts$`)

const Version = "0.2.1"

type Server struct {
	cfg       Config
	manager   *SessionManager
	provider  Provider
	policy    AdPolicy
	ready     atomic.Bool
	mu        sync.Mutex
	rates     map[string][]time.Time
	gate      chan struct{}
	upstream  *http.Client
	downloads *downloadManager
	mediaGate chan struct{}
}

func New(c Config, p Provider) (*Server, error) {
	policy := FilterPause{On: c.Adblock}
	m := NewManager(c, p, policy)
	if e := m.Prepare(); e != nil {
		m.Close()
		return nil, e
	}
	s := &Server{cfg: c, provider: p, manager: m, policy: policy, rates: map[string][]time.Time{}, gate: make(chan struct{}, 64)}
	s.mediaGate = make(chan struct{}, 4)
	s.upstream = upstreamClient()
	s.downloads = newDownloadManager(s)
	s.checkReady()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				s.checkReady()
			}
		}
	}()
	return s, nil
}
func (s *Server) checkReady() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.ready.Store(s.provider.Ready(ctx) && exec.CommandContext(ctx, s.cfg.FFmpeg, "-version").Run() == nil)
}
func (s *Server) Close() {
	s.downloads.close()
	s.manager.Close()
	if t, ok := s.upstream.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, code, msg string, retry bool) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg, "retryable": retry}})
}
func (s *Server) principal(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || len(header) > 1024 {
		return ""
	}
	got := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
	owner := ""
	for p, t := range s.cfg.Tokens {
		want := sha256.Sum256([]byte(t))
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			owner = p
		}
	}
	return owner
}
func (s *Server) limited(owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	old := s.rates[owner]
	keep := old[:0]
	for _, t := range old {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= 10 {
		s.rates[owner] = keep
		return true
	}
	s.rates[owner] = append(keep, now)
	return false
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	default:
		w.Header().Set("Retry-After", "1")
		problem(w, 429, "rate_limited", "Request capacity reached", true)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if len(r.URL.RawQuery) > 2048 || strings.Contains(r.URL.EscapedPath(), "%") || strings.Contains(r.URL.Path, "..") || strings.Contains(r.URL.Path, "//") {
		problem(w, 400, "invalid_request", "Invalid path", false)
		return
	}
	p := r.URL.Path
	if s.cfg.Prefix != "" {
		if !strings.HasPrefix(p, s.cfg.Prefix+"/") {
			problem(w, 404, "content_unavailable", "Route unavailable", false)
			return
		}
		p = strings.TrimPrefix(p, s.cfg.Prefix)
	}
	if p == "/v1/health" && r.Method == "GET" {
		status := "ok"
		if !s.ready.Load() {
			status = "degraded"
		}
		writeJSON(w, 200, map[string]any{"status": status, "server_version": Version, "api_version": "v1"})
		return
	}
	if p == "/v1/capabilities" && r.Method == "GET" {
		writeJSON(w, 200, map[string]any{"api_version": "v1", "playback_protocols": []string{"hls"}, "live": s.ready.Load() && s.provider.Live(), "vod": s.recorded(), "clips": s.recorded(), "quality_selection": s.ready.Load() && s.provider.Live(), "downloads": s.recorded(), "authorized_downloads_only": !s.recorded(), "client_side_downloads": s.recorded(), "ad_handling": s.policy.Mode()})
		return
	}
	if strings.HasPrefix(p, "/media/") {
		if r.Method != "GET" && r.Method != "HEAD" {
			problem(w, 400, "invalid_request", "Unsupported method", false)
			return
		}
		s.media(w, r, p)
		return
	}
	owner := s.principal(r)
	if owner == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		problem(w, 401, "authentication_required", "Source authentication required", false)
		return
	}
	if strings.HasPrefix(p, "/v1/downloads") {
		s.downloadRoute(w, r, owner, p)
		return
	}
	if p == "/v1/playback/sessions" && r.Method == "POST" {
		s.create(w, r, owner)
		return
	}
	if strings.HasPrefix(p, "/v1/playback/sessions/") && r.Method == "DELETE" {
		id := strings.TrimPrefix(p, "/v1/playback/sessions/")
		if !identifier.MatchString(id) {
			problem(w, 400, "invalid_request", "Invalid identifier", false)
			return
		}
		if p := s.manager.owner(id); p != "" && p != owner {
			problem(w, 403, "permission_required", "Session belongs to another principal", false)
			return
		}
		s.manager.remove(id)
		w.WriteHeader(204)
		return
	}
	problem(w, 404, "content_unavailable", "Route unavailable", false)
}
func (s *Server) create(w http.ResponseWriter, r *http.Request, owner string) {
	var req struct {
		ContentType string  `json:"content_type"`
		Channel     string  `json:"channel_login"`
		ID          string  `json:"content_id"`
		Quality     *string `json:"requested_quality"`
		Preferences struct {
			LowLatency bool `json:"low_latency"`
		} `json:"preferences"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	if e := d.Decode(&req); e != nil {
		problem(w, 400, "invalid_request", "Invalid JSON body", false)
		return
	}
	if d.Decode(&struct{}{}) != io.EOF {
		problem(w, 400, "invalid_request", "One JSON object required", false)
		return
	}
	target := strings.ToLower(req.Channel)
	if req.ContentType == "vod" || req.ContentType == "clip" {
		valid := identifier.MatchString(req.ID)
		if req.ContentType == "vod" {
			valid = vodIDPattern.MatchString(req.ID)
		}
		if !valid || req.Quality == nil || len(*req.Quality) > 40 || *req.Quality == "" {
			problem(w, 400, "invalid_request", "Content ID and requested quality required", false)
			return
		}
		if !s.recorded() {
			problem(w, 400, "unsupported_capability", "Recorded playback unavailable", false)
			return
		}
		target = req.ContentType + ":" + req.ID
	} else if req.ContentType != "live" || !loginPattern.MatchString(req.Channel) || req.Quality == nil || len(*req.Quality) > 40 || *req.Quality == "" {
		problem(w, 400, "invalid_request", "Live channel and requested quality required", false)
		return
	}
	if !s.ready.Load() {
		problem(w, 503, "source_unavailable", "Required media components unavailable", true)
		return
	}
	if s.limited(owner) || !s.manager.reserve(owner) {
		w.Header().Set("Retry-After", "60")
		problem(w, 429, "rate_limited", "Session limit reached", true)
		return
	}
	defer s.manager.unreserve(owner)
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Startup)
	defer cancel()
	probe, pcancel := context.WithTimeout(ctx, 20*time.Second)
	qs, e := s.provider.Qualities(probe, target)
	pcancel()
	if e != nil {
		var limit UpstreamLimit
		if errors.As(e, &limit) {
			w.Header().Set("Retry-After", strconv.Itoa(limit.After))
			problem(w, 429, "rate_limited", "Upstream rate limit reached", true)
		} else if e == ErrUnavailable {
			problem(w, 404, "content_unavailable", "Stream is offline or unavailable", false)
		} else {
			problem(w, 503, "source_unavailable", "Upstream acquisition failed; see operator documentation", true)
		}
		return
	}
	found := false
	for _, q := range qs {
		if q == *req.Quality {
			found = true
		}
	}
	if !found {
		problem(w, 400, "invalid_request", "Requested quality unavailable", false)
		return
	}
	chosenQuality := *req.Quality
	if chosenQuality == "auto" {
		chosenQuality = qs[len(qs)-1]
	}
	var session *Session
	if req.ContentType == "vod" {
		session, e = s.manager.startRecorded(ctx, owner, target, chosenQuality)
	} else {
		session, e = s.manager.start(ctx, owner, target, chosenQuality)
	}
	if e != nil {
		problem(w, 503, "source_unavailable", "Playback could not start within bounded startup time", true)
		return
	}
	filtering := "disabled"
	if s.policy.Enabled() {
		filtering = "enabled"
	}
	playerCaps := []string{"pause", "pip"}
	if req.ContentType != "live" {
		playerCaps = append(playerCaps, "seek")
	}
	writeJSON(w, 201, map[string]any{"session_id": session.ID, "hls_url": s.signedURL(session, "master.m3u8"), "qualities": qs, "selected_quality": chosenQuality, "expires_at": session.Expires.Format(time.RFC3339), "player_capabilities": playerCaps, "filtering_status": filtering, "state": "playing"})
}
func (s *Server) signature(id, resource string, exp int64) string {
	h := hmac.New(sha256.New, s.cfg.Key)
	fmt.Fprintf(h, "%s\n%s\n%d", id, resource, exp)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func (s *Server) signedURL(session *Session, resource string) string {
	exp := session.Expires.Unix()
	return fmt.Sprintf("%s/media/%s/%s?exp=%d&sig=%s", s.cfg.BaseURL, session.ID, resource, exp, s.signature(session.ID, resource, exp))
}

// MediaGateway only serves the remuxer's bounded local artifacts. It never
// accepts a URL or forwards an upstream manifest to AVPlayer.
func (s *Server) media(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(strings.TrimPrefix(path, "/media/"), "/")
	if len(parts) != 2 || !identifier.MatchString(parts[0]) {
		problem(w, 404, "content_unavailable", "Media unavailable", false)
		return
	}
	id, resource := parts[0], parts[1]
	if resource != "master.m3u8" && resource != "index.m3u8" && !segmentPattern.MatchString(resource) && !initPattern.MatchString(resource) {
		problem(w, 404, "content_unavailable", "Media unavailable", false)
		return
	}
	q := r.URL.Query()
	exp, e := strconv.ParseInt(q.Get("exp"), 10, 64)
	sig := q.Get("sig")
	expected := s.signature(id, resource, exp)
	if e != nil || len(q["exp"]) != 1 || len(q["sig"]) != 1 || exp <= time.Now().Unix() || !hmac.Equal([]byte(sig), []byte(expected)) {
		problem(w, 403, "permission_required", "Media signature invalid or expired", false)
		return
	}
	sess := s.manager.get(id)
	if sess == nil || sess.Expires.Unix() != exp {
		problem(w, 404, "content_unavailable", "Session unavailable", false)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if resource == "master.m3u8" {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=12000000\n%s\n", s.signedURL(sess, "index.m3u8"))
		return
	}
	if sess.Plan != nil {
		s.recordedMedia(w, r, sess, resource)
		return
	}
	if resource == "index.m3u8" {
		b, e := os.ReadFile(filepath.Join(sess.Dir, resource))
		if e != nil || len(b) > 65536 {
			problem(w, 503, "source_unavailable", "Media buffering", true)
			return
		}
		var out strings.Builder
		lines := strings.Split(string(b), "\n")
		hasStart := false
		for _, line := range lines {
			if line == "#EXT-X-DISCONTINUITY" {
				hasStart = true
			}
		}
		for _, line := range lines {
			if line == "" {
				continue
			}
			if line == "#EXTM3U" {
				out.WriteString(line + "\n")
				seq := 1
				if hasStart {
					seq = 0
				}
				fmt.Fprintf(&out, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", seq)
				continue
			}
			if strings.HasPrefix(line, "#") {
				out.WriteString(line + "\n")
				continue
			}
			if !segmentPattern.MatchString(line) {
				problem(w, 503, "source_unavailable", "Unsupported local media", false)
				return
			}
			out.WriteString(s.signedURL(sess, line) + "\n")
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(w, out.String())
		return
	}
	f, e := os.Open(filepath.Join(sess.Dir, resource))
	if e != nil {
		problem(w, 404, "content_unavailable", "Segment unavailable", false)
		return
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Size() > s.cfg.MaxDisk {
		problem(w, 404, "content_unavailable", "Segment unavailable", false)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	http.ServeContent(w, r, resource, info.ModTime(), f)
}

func (s *Server) recorded() bool {
	p, ok := s.provider.(RecordedProvider)
	return ok && p.Recorded() && s.ready.Load()
}
