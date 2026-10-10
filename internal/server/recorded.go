package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type RecordedPlan struct {
	Playlist  string            `json:"playlist"`
	Resources map[string]string `json:"resources"`
	Duration  float64           `json:"duration"`
}
type RecordedProvider interface {
	Recorded() bool
	Plan(context.Context, string, string, bool) (RecordedPlan, error)
}

func (p StreamlinkProvider) Recorded() bool { return true }
func (p StreamlinkProvider) Plan(ctx context.Context, target, quality string, ads bool) (RecordedPlan, error) {
	mode := "off"
	if ads {
		mode = "on"
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Python, p.Worker, "plan", "--channel", target, "--quality", quality, "--adblock", mode)
	if p.CacheDir != "" {
		cmd.Env = append(os.Environ(), "RESOLVER_CACHE_DIR="+p.CacheDir)
	}
	process(cmd)
	// Metadata only: private upstream URLs never enter a client response or log.
	b, err := cmd.StdoutPipe()
	if err != nil {
		return RecordedPlan{}, ErrProvider
	}
	if cmd.Start() != nil {
		return RecordedPlan{}, ErrProvider
	}
	data, err := io.ReadAll(io.LimitReader(b, 16*1024*1024+1))
	if err != nil || len(data) > 16*1024*1024 {
		cancel()
	}
	wait := cmd.Wait()
	var plan RecordedPlan
	if err != nil || wait != nil || len(data) > 16*1024*1024 || json.Unmarshal(data, &plan) != nil || len(plan.Resources) == 0 || len(plan.Resources) > 40000 || len(plan.Playlist) > 4*1024*1024 || plan.Duration <= 0 {
		return plan, ErrProvider
	}
	for name, raw := range plan.Resources {
		if (!segmentPattern.MatchString(name) && !initPattern.MatchString(name)) || !allowedUpstream(raw) {
			return RecordedPlan{}, ErrProvider
		}
	}
	return plan, nil
}
func (m *SessionManager) startRecorded(ctx context.Context, owner, target, quality string) (*Session, error) {
	p, ok := m.provider.(RecordedProvider)
	if !ok {
		return nil, ErrProvider
	}
	plan, err := p.Plan(ctx, target, quality, m.policy.Enabled())
	if err != nil {
		return nil, err
	}
	id := randomID()
	dir := filepath.Join(m.cfg.MediaDir, id)
	if err = os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(m.ctx)
	sess := &Session{ID: id, Owner: owner, Dir: dir, Expires: time.Now().UTC().Add(m.cfg.Lifetime).Truncate(time.Second), Last: time.Now(), cancel: cancel, done: make(chan struct{}), Plan: &plan}
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		cancel()
		os.RemoveAll(dir)
		return nil, m.ctx.Err()
	}
	m.sessions[id] = sess
	m.wg.Add(1)
	m.mu.Unlock()
	go func() { defer m.wg.Done(); defer close(sess.done); defer os.RemoveAll(dir); <-life.Done() }()
	return sess, nil
}
func allowedUpstream(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Hostname() == "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range []string{"twitch.tv", "ttvnw.net", "twitchcdn.net", "jtvnw.net", "cloudfront.net"} {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}
func publicAddress(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return false
		}
	}
	return true
}
func upstreamClient() *http.Client {
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: 15 * time.Second, MaxIdleConns: 32, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" {
			return nil, errors.New("destination denied")
		}
		if !allowedUpstream("https://" + address) {
			return nil, errors.New("destination denied")
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("resolution failed")
		}
		for _, ip := range ips {
			if !publicAddress(ip.IP) {
				return nil, errors.New("destination denied")
			}
		}
		for _, ip := range ips {
			conn, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if e == nil {
				return conn, nil
			}
		}
		return nil, errors.New("connection failed")
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 || !allowedUpstream(req.URL.String()) {
			return errors.New("redirect denied")
		}
		return nil
	}}
}
func (s *Server) recordedMedia(w http.ResponseWriter, r *http.Request, sess *Session, resource string) {
	if resource == "index.m3u8" {
		var out strings.Builder
		for _, line := range strings.Split(sess.Plan.Playlist, "\n") {
			if strings.HasPrefix(line, "#EXT-X-MAP:URI=\"") {
				name := strings.TrimSuffix(strings.TrimPrefix(line, "#EXT-X-MAP:URI=\""), "\"")
				if _, ok := sess.Plan.Resources[name]; !ok {
					problem(w, 503, "source_unavailable", "Invalid recorded playlist", false)
					return
				}
				fmt.Fprintf(&out, "#EXT-X-MAP:URI=\"%s\"\n", s.signedURL(sess, name))
				continue
			}
			if line != "" && !strings.HasPrefix(line, "#") {
				if _, ok := sess.Plan.Resources[line]; !ok {
					problem(w, 503, "source_unavailable", "Invalid recorded playlist", false)
					return
				}
				line = s.signedURL(sess, line)
			}
			out.WriteString(line + "\n")
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		io.WriteString(w, out.String())
		return
	}
	select {
	case s.mediaGate <- struct{}{}:
		defer func() { <-s.mediaGate }()
	case <-r.Context().Done():
		return
	}
	raw, ok := sess.Plan.Resources[resource]
	if !ok || !allowedUpstream(raw) {
		problem(w, 404, "content_unavailable", "Resource unavailable", false)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", raw, nil)
	if err != nil {
		problem(w, 503, "source_unavailable", "Recorded media unavailable", true)
		return
	}
	res, err := s.upstream.Do(req)
	if err != nil || res.StatusCode != 200 {
		if res != nil {
			res.Body.Close()
		}
		problem(w, 503, "source_unavailable", "Recorded media unavailable", true)
		return
	}
	defer res.Body.Close()
	limit := int64(32 * 1024 * 1024)
	mime := "video/mp2t"
	if initPattern.MatchString(resource) {
		limit = 1024 * 1024
		mime = "video/mp4"
	} else if res.Header.Get("Content-Type") == "video/mp4" {
		mime = "video/mp4"
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		problem(w, 503, "source_unavailable", "Resource limit exceeded", false)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}
