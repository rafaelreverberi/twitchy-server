package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type downloadJob struct {
	ID                     string   `json:"id"`
	Status                 string   `json:"status"`
	Progress               float64  `json:"progress"`
	Bytes                  int64    `json:"transferred_bytes"`
	Filename               string   `json:"filename"`
	Controls               []string `json:"controls"`
	Error                  any      `json:"error,omitempty"`
	owner, target, quality string
	expires                time.Time
	cancel                 context.CancelFunc
}
type downloadManager struct {
	mu     sync.Mutex
	jobs   map[string]*downloadJob
	s      *Server
	closed bool
	wg     sync.WaitGroup
}

func newDownloadManager(s *Server) *downloadManager {
	return &downloadManager{s: s, jobs: map[string]*downloadJob{}}
}
func (d *downloadManager) close() {
	d.mu.Lock()
	d.closed = true
	for _, j := range d.jobs {
		if j.cancel != nil {
			j.cancel()
		}
	}
	d.mu.Unlock()
	d.wg.Wait()
}
func (d *downloadManager) sweep() {
	for id, j := range d.jobs {
		if j.cancel == nil && time.Now().After(j.expires) {
			delete(d.jobs, id)
		}
	}
}
func (s *Server) downloadRoute(w http.ResponseWriter, r *http.Request, owner, path string) {
	if !s.recorded() {
		problem(w, 400, "unsupported_capability", "Recorded downloads unavailable", false)
		return
	}
	d := s.downloads
	if path == "/v1/downloads" && r.Method == "POST" {
		var req struct {
			Type       string `json:"content_type"`
			ID         string `json:"content_id"`
			Quality    string `json:"requested_quality"`
			Permission string `json:"authorization_reference"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF || (req.Type != "vod" && req.Type != "clip") || !identifier.MatchString(req.ID) || (req.Type == "vod" && !vodIDPattern.MatchString(req.ID)) {
			problem(w, 400, "invalid_request", "Recorded content ID required", false)
			return
		}
		if req.Quality == "" {
			req.Quality = "auto"
		}
		if len(req.Quality) > 40 {
			problem(w, 400, "invalid_request", "Invalid quality", false)
			return
		}
		if s.limited(owner) {
			problem(w, 429, "rate_limited", "Creation limit reached", true)
			return
		}
		select {
		case s.manager.recordingGate <- struct{}{}:
			defer func() { <-s.manager.recordingGate }()
		case <-r.Context().Done():
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		target := req.Type + ":" + req.ID
		qualities, err := s.provider.Qualities(ctx, target)
		if err != nil {
			problem(w, 503, "source_unavailable", "Recorded acquisition failed", true)
			return
		}
		quality := req.Quality
		valid := false
		for _, q := range qualities {
			if q == quality {
				valid = true
			}
		}
		if !valid {
			problem(w, 400, "invalid_request", "Requested quality unavailable", false)
			return
		}
		if quality == "auto" {
			quality = qualities[len(qualities)-1]
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.sweep()
		count := 0
		for _, j := range d.jobs {
			if j.owner == owner {
				count++
			}
		}
		if d.closed || len(d.jobs) >= 32 || count >= 8 {
			problem(w, 429, "rate_limited", "Download job capacity reached", true)
			return
		}
		job := &downloadJob{ID: randomID(), Status: "ready", Filename: req.Type + "-" + req.ID + ".mp4", Controls: []string{"cancel", "retry"}, owner: owner, target: target, quality: quality, expires: time.Now().Add(time.Hour)}
		d.jobs[job.ID] = job
		writeJSON(w, 202, job)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/v1/downloads/"), "/")
	if len(parts) < 1 || len(parts) > 2 || !identifier.MatchString(parts[0]) {
		problem(w, 404, "content_unavailable", "Job unavailable", false)
		return
	}
	id := parts[0]
	d.mu.Lock()
	d.sweep()
	j := d.jobs[id]
	if j != nil && j.owner != owner {
		d.mu.Unlock()
		problem(w, 403, "permission_required", "Job belongs to another principal", false)
		return
	}
	if r.Method == "DELETE" && len(parts) == 1 {
		if j != nil {
			if j.cancel != nil {
				j.cancel()
			}
			j.Status = "cancelled"
			j.expires = time.Now().Add(time.Minute)
		}
		d.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	if j == nil {
		d.mu.Unlock()
		problem(w, 404, "content_unavailable", "Job unavailable", false)
		return
	}
	if r.Method == "GET" && len(parts) == 1 {
		writeJSON(w, 200, j)
		d.mu.Unlock()
		return
	}
	if r.Method != "GET" || len(parts) != 2 || parts[1] != "file" {
		d.mu.Unlock()
		problem(w, 404, "content_unavailable", "Route unavailable", false)
		return
	}
	if d.closed || j.cancel != nil || j.Status == "cancelled" {
		d.mu.Unlock()
		problem(w, 409, "invalid_state", "Download unavailable or already transferring", false)
		return
	}
	if !s.manager.reserve(owner) {
		d.mu.Unlock()
		problem(w, 429, "rate_limited", "Transfer capacity reached", true)
		return
	}
	defer s.manager.unreserve(owner)
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Hour)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(s.cfg.Startup + 15*time.Second))
	writer := transferWriter{w: w, controller: controller}
	j.cancel = cancel
	j.Status = "transferring"
	j.Error = nil
	d.wg.Add(1)
	d.mu.Unlock()
	stop := context.AfterFunc(s.manager.ctx, cancel)
	defer stop()
	defer cancel()
	defer d.wg.Done()
	select {
	case s.manager.recordingGate <- struct{}{}:
		defer func() { <-s.manager.recordingGate }()
	case <-ctx.Done():
		s.downloadFailed(id)
		return
	}
	cmd := exec.CommandContext(ctx, s.cfg.FFmpeg, "-v", "error", "-fflags", "+genpts+discardcorrupt", "-dts_delta_threshold", "1", "-protocol_whitelist", "pipe", "-f", "mpegts", "-i", "pipe:0", "-map", "0:v:0", "-map", "0:a:0?", "-c", "copy", "-bsf:a", "aac_adtstoasc,setts=ts='if(eq(N,0),TS,max(TS,PREV_OUTDTS+DURATION))'", "-avoid_negative_ts", "make_zero", "-movflags", "frag_keyframe+empty_moov+default_base_moof", "-frag_duration", "2000000", "-f", "mp4", "pipe:1")
	process(cmd)
	input, err := cmd.StdinPipe()
	if err != nil {
		s.downloadFailed(id)
		problem(w, 503, "source_unavailable", "Transfer startup failed", true)
		return
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		s.downloadFailed(id)
		problem(w, 503, "source_unavailable", "Transfer startup failed", true)
		return
	}
	if cmd.Start() != nil {
		input.Close()
		s.downloadFailed(id)
		problem(w, 503, "source_unavailable", "Transfer startup failed", true)
		return
	}
	produced := make(chan error, 1)
	go func() {
		err := s.provider.Stream(ctx, j.target, j.quality, s.policy.Enabled(), input)
		input.Close()
		produced <- err
	}()
	first := make([]byte, 4096)
	n, readErr := output.Read(first)
	if n > 0 {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", j.Filename))
		w.Header().Set("Cache-Control", "no-store")
		_, readErr = writer.Write(first[:n])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	var copied int64
	var copyErr error
	if n > 0 {
		copied, copyErr = io.Copy(writer, output)
	}
	if copyErr != nil || (readErr != nil && readErr != io.EOF) {
		cancel()
	}
	waitErr := cmd.Wait()
	cancel()
	providerErr := <-produced
	d.mu.Lock()
	j.cancel = nil
	j.expires = time.Now().Add(time.Hour)
	success := j.Status != "cancelled" && n > 0 && copyErr == nil && waitErr == nil && providerErr == nil && (readErr == nil || readErr == io.EOF)
	if success {
		j.Status = "completed"
		j.Bytes = int64(n) + copied
		j.Progress = 1
	} else if j.Status != "cancelled" {
		if j.Status != "cancelled" {
			j.Status = "failed"
		}
		j.Error = map[string]any{"code": "source_unavailable", "message": "Transfer failed; retry starts a new client download", "retryable": true}
	}
	d.mu.Unlock()
	if !success {
		if n == 0 {
			problem(w, 503, "source_unavailable", "Transfer could not start", true)
		} else {
			panic(http.ErrAbortHandler)
		}
	}
}
func (s *Server) downloadFailed(id string) {
	s.downloads.mu.Lock()
	defer s.downloads.mu.Unlock()
	if j := s.downloads.jobs[id]; j != nil {
		j.cancel = nil
		if j.Status != "cancelled" {
			j.Status = "failed"
		}
		j.Error = map[string]any{"code": "source_unavailable", "message": "Transfer failed", "retryable": true}
	}
}

// Refresh the socket deadline on progress, rather than limiting the duration of
// an entire archive transfer to the short playback-startup timeout.
type transferWriter struct {
	w          http.ResponseWriter
	controller *http.ResponseController
}

func (w transferWriter) Write(p []byte) (int, error) {
	_ = w.controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return w.w.Write(p)
}
