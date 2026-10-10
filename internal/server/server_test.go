package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{BaseURL: "http://localhost", Prefix: "", Python: "python3", Worker: "none", FFmpeg: "ffmpeg", MediaDir: filepath.Join(t.TempDir(), "media"), Tokens: map[string]string{"alice": strings.Repeat("a", 43), "bob": strings.Repeat("b", 43)}, Key: bytes.Repeat([]byte{3}, 32), Adblock: true, Development: true, MaxSessions: 3, PerPrincipal: 1, Lifetime: 12 * time.Hour, Idle: 2 * time.Minute, Startup: 8 * time.Second, MaxDisk: 8 << 20}
}

type stubProvider struct {
	ready bool
	err   error
}

func (p stubProvider) Ready(context.Context) bool { return p.ready }
func (p stubProvider) Live() bool                 { return false }
func (p stubProvider) Qualities(context.Context, string) ([]string, error) {
	return []string{"auto", "360p"}, p.err
}
func (p stubProvider) Stream(ctx context.Context, _ string, _ string, _ bool, _ io.Writer) error {
	<-ctx.Done()
	return ctx.Err()
}
func request(t *testing.T, s http.Handler, method, path, owner, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if owner != "" {
		r.Header.Set("Authorization", "Bearer "+strings.Repeat(owner, 43))
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

var recordingsMu sync.Mutex
var recordings = []map[string]any{}

func record(t *testing.T, route, method string, w *httptest.ResponseRecorder) {
	t.Helper()
	var body any
	if w.Code != 204 {
		if e := json.Unmarshal(w.Body.Bytes(), &body); e != nil {
			t.Fatal(e)
		}
	}
	recordingsMu.Lock()
	defer recordingsMu.Unlock()
	recordings = append(recordings, map[string]any{"route": route, "method": method, "status": w.Code, "body": body})
}
func TestMain(m *testing.M) {
	code := m.Run()
	if path := os.Getenv("CONTRACT_REPORT"); path != "" {
		b, e := json.MarshalIndent(recordings, "", "  ")
		if e != nil {
			panic(e)
		}
		if e = os.WriteFile(path, b, 0600); e != nil {
			panic(e)
		}
	}
	os.Exit(code)
}
func TestControl(t *testing.T) {
	c := testConfig(t)
	s, e := New(c, stubProvider{ready: true, err: ErrUnavailable})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	for _, p := range []string{"health", "capabilities"} {
		w := request(t, s, "GET", "/v1/"+p, "", "")
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		record(t, "/v1/"+p, "get", w)
	}
	body := `{"content_type":"live","channel_login":"example","requested_quality":"auto"}`
	cases := []struct {
		path, method, auth, body string
		status                   int
	}{{"/v1/playback/sessions", "POST", "", body, 401}, {"/v1/playback/sessions", "POST", "a", body, 404}, {"/v1/playback/sessions", "POST", "a", `{"content_type":"live","channel_login":"http://127.0.0.1","requested_quality":"auto"}`, 400}, {"/v1/playback/sessions", "POST", "a", `{}`, 400}, {"/v1/playback/sessions", "POST", "a", body + body, 400}, {"/v1/playback/sessions", "POST", "a", `{"content_type":"vod","content_id":"1","requested_quality":"auto"}`, 400}, {"/v1/downloads", "POST", "a", `{}`, 400}, {"/v1/downloads/id", "GET", "a", "", 400}, {"/v1/downloads/id/file", "GET", "a", "", 400}, {"/v1/downloads/id", "DELETE", "a", "", 400}, {"/v1/playback/sessions/id", "DELETE", "a", "", 204}}
	for _, tt := range cases {
		w := request(t, s, tt.method, tt.path, tt.auth, tt.body)
		if w.Code != tt.status {
			t.Fatalf("%s %d %s", tt.path, w.Code, w.Body.String())
		}
		route := tt.path
		if strings.HasPrefix(route, "/v1/playback/sessions/") {
			route = "/v1/playback/sessions/{sessionId}"
		}
		if strings.HasPrefix(route, "/v1/downloads/id") {
			route = strings.Replace(route, "/id", "/{downloadId}", 1)
		}
		record(t, route, strings.ToLower(tt.method), w)
	}
	// Error results and reservations do not leak sessions/workers.
	for i := 0; i < 11; i++ {
		w := request(t, s, "POST", "/v1/playback/sessions", "a", body)
		if i == 10 && w.Code != 429 {
			t.Fatal(w.Code)
		}
	}
	if len(s.manager.sessions) != 0 || len(s.manager.pending) != 0 {
		t.Fatal("reservation leak")
	}
	w := request(t, s, "POST", "/v1/playback/sessions", "a", body)
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" {
		t.Fatal(w.Code)
	}
	record(t, "/v1/playback/sessions", "post", w)
}
func TestDegradedAndStartupFailure(t *testing.T) {
	for _, ready := range []bool{false, true} {
		c := testConfig(t)
		c.Startup = 300 * time.Millisecond
		s, e := New(c, stubProvider{ready: ready})
		if e != nil {
			t.Fatal(e)
		}
		w := request(t, s, "POST", "/v1/playback/sessions", "a", `{"content_type":"live","channel_login":"example","requested_quality":"auto"}`)
		if w.Code != 503 {
			t.Fatal(w.Code)
		}
		record(t, "/v1/playback/sessions", "post", w)
		if ready {
			time.Sleep(100 * time.Millisecond)
			if entries, _ := os.ReadDir(c.MediaDir); len(entries) != 0 {
				t.Fatal("startup leaked media")
			}
		}
		s.Close()
	}
}
func TestUpstreamLimitResponse(t *testing.T) {
	c := testConfig(t)
	s, e := New(c, stubProvider{ready: true, err: UpstreamLimit{After: 17}})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	w := request(t, s, "POST", "/v1/playback/sessions", "a", `{"content_type":"live","channel_login":"example","requested_quality":"auto"}`)
	if w.Code != 429 || w.Header().Get("Retry-After") != "17" {
		t.Fatal("upstream limit not propagated")
	}
	record(t, "/v1/playback/sessions", "post", w)
}

func TestSignedHLSOwnershipRevocationAndTTL(t *testing.T) {
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Fatal("ffmpeg required for integration test")
	}
	dir := t.TempDir()
	media := filepath.Join(dir, "normal.ts")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25", "-f", "lavfi", "-i", "sine=frequency=440", "-t", "4", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-threads", "1", "-g", "25", "-c:a", "aac", "-f", "mpegts", media)
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("%s %v", b, e)
	}
	c := testConfig(t)
	c.Prefix = "/source"
	c.BaseURL += "/source"
	s, e := New(c, FixtureProvider{File: media})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	host := httptest.NewServer(s)
	defer host.Close()
	s.cfg.BaseURL = host.URL + c.Prefix
	w := request(t, s, "POST", "/source/v1/playback/sessions", "a", `{"content_type":"live","channel_login":"fixture","requested_quality":"auto"}`)
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	record(t, "/v1/playback/sessions", "post", w)
	var response struct {
		ID              string `json:"session_id"`
		URL             string `json:"hls_url"`
		SelectedQuality string `json:"selected_quality"`
	}
	if json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal("decode")
	}
	if response.SelectedQuality != "360p" {
		t.Fatal("server did not report resolved best quality")
	}
	get := func(url string) (int, string) {
		t.Helper()
		r, e := http.Get(url)
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	status, master := get(response.URL)
	if status != 200 || strings.Contains(master, "ttvnw") {
		t.Fatal("invalid master")
	}
	variant := ""
	for _, line := range strings.Split(master, "\n") {
		if strings.HasPrefix(line, "http") {
			variant = line
		}
	}
	status, playlist := get(variant)
	if status != 200 || !strings.Contains(playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:") {
		t.Fatal(playlist)
	}
	var segmentURL string
	for _, line := range strings.Split(playlist, "\n") {
		if strings.HasPrefix(line, "http") {
			segmentURL = line
			break
		}
	}
	status, b := get(segmentURL)
	if status != 200 || len(b) < 188 || b[0] != 0x47 {
		t.Fatal("no TS bytes")
	}
	// Whole HLS pipeline: decoder resolves our master, variant and signed segments.
	decode := exec.Command("ffmpeg", "-v", "error", "-i", response.URL, "-t", "2", "-f", "null", "-")
	if out, e := decode.CombinedOutput(); e != nil {
		t.Fatalf("HLS decode failed (output length %d): %v", len(out), e)
	}
	if status, _ := get(response.URL + "x"); status != 403 {
		t.Fatal("tampered signature accepted")
	}
	if status, _ := get(strings.Replace(response.URL, "master.m3u8", "index.m3u8", 1)); status != 403 {
		t.Fatal("cross-resource signature accepted")
	}
	if status, _ := get(host.URL + "/source/media/" + response.ID + "/%2e%2e?exp=1&sig=x"); status != 400 {
		t.Fatal("traversal accepted")
	}
	other := request(t, s, "DELETE", "/source/v1/playback/sessions/"+response.ID, "b", "")
	if other.Code != 403 {
		t.Fatal("ownership failure")
	}
	record(t, "/v1/playback/sessions/{sessionId}", "delete", other)
	limit := request(t, s, "POST", "/source/v1/playback/sessions", "a", `{"content_type":"live","channel_login":"fixture","requested_quality":"auto"}`)
	if limit.Code != 429 {
		t.Fatal("principal limit")
	}
	del := request(t, s, "DELETE", "/source/v1/playback/sessions/"+response.ID, "a", "")
	if del.Code != 204 || del.Body.Len() != 0 {
		t.Fatal("delete failure")
	}
	if status, _ := get(response.URL); status != 404 {
		t.Fatal("revocation failed")
	}
	if entries, _ := os.ReadDir(c.MediaDir); len(entries) != 0 {
		t.Fatal("files leaked")
	}
	// Idle and absolute expiry are independent. Sweep uses the same cleanup path.
	for _, expiry := range []bool{false, true} {
		w = request(t, s, "POST", "/source/v1/playback/sessions", "a", `{"content_type":"live","channel_login":"fixture","requested_quality":"auto"}`)
		if w.Code != 201 {
			t.Fatal(w.Code)
		}
		_ = json.Unmarshal(w.Body.Bytes(), &response)
		s.manager.mu.Lock()
		sess := s.manager.sessions[response.ID]
		if expiry {
			sess.Expires = time.Now().Add(-time.Second)
		} else {
			sess.Last = time.Now().Add(-c.Idle - time.Second)
		}
		s.manager.mu.Unlock()
		s.manager.sweep(time.Now())
		if s.manager.owner(response.ID) != "" {
			t.Fatal("TTL failed")
		}
	}
}
func TestConfigValidation(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "tokens")
	kf := filepath.Join(dir, "key")
	_ = os.WriteFile(tf, []byte(`{"local":"`+strings.Repeat("a", 43)+`"}`), 0600)
	_ = os.WriteFile(kf, bytes.Repeat([]byte{4}, 32), 0600)
	t.Setenv("TOKEN_FILE", tf)
	t.Setenv("SIGNING_KEY_FILE", kf)
	t.Setenv("PUBLIC_BASE_URL", "http://localhost:8080/source")
	t.Setenv("ALLOW_HTTP", "true")
	if c, e := LoadConfig(); e != nil || c.Prefix != "/source" {
		t.Fatal(e)
	}
	for _, url := range []string{"https://user:pass@example.org", "https://example.org?token=x", "http://example.org/../secret", "https://example.org#x"} {
		t.Setenv("PUBLIC_BASE_URL", url)
		if _, e := LoadConfig(); e == nil {
			t.Fatal("invalid base accepted")
		}
	}
}

func TestSignatureExpiryCapacityAndFailureCleanup(t *testing.T) {
	c := testConfig(t)
	c.Startup = 200 * time.Millisecond
	s, e := New(c, stubProvider{ready: true})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	exp := time.Now().Add(-time.Second).Unix()
	path := "/media/test/master.m3u8?exp=" + fmt.Sprint(exp) + "&sig=" + s.signature("test", "master.m3u8", exp)
	if w := request(t, s, "GET", path, "", ""); w.Code != 403 {
		t.Fatal("expired signature accepted")
	}
	r := httptest.NewRequest("POST", "/v1/playback/sessions", strings.NewReader(`{"content_type":"live","channel_login":"fixture","requested_quality":"auto"}`))
	r.Header.Set("Authorization", "Bearer "+c.Tokens["alice"])
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	if entries, _ := os.ReadDir(c.MediaDir); len(entries) != 0 {
		t.Fatal("client cancellation leaked files")
	}
	if !s.manager.reserve("alice") || s.manager.reserve("alice") {
		t.Fatal("pending reservation principal limit")
	}
	s.manager.unreserve("alice")
	for i := 0; i < cap(s.gate); i++ {
		s.gate <- struct{}{}
	}
	w = request(t, s, "GET", "/v1/health", "", "")
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("request limit")
	}
	for len(s.gate) > 0 {
		<-s.gate
	}
}

func TestResolverReacquisitionBackoffAndCancellation(t *testing.T) {
	python, e := exec.LookPath("python3")
	if e != nil {
		t.Fatal(e)
	}
	script := filepath.Join(t.TempDir(), "worker.py")
	if e := os.WriteFile(script, []byte("import sys\nif sys.argv[1]=='probe': print('{\"qualities\":[\"auto\",\"360p\"]}')\nelif sys.argv[1]=='check': pass\nelse: sys.stdout.buffer.write(b'NORMAL'); sys.stdout.buffer.flush()\n"), 0600); e != nil {
		t.Fatal(e)
	}
	p := StreamlinkProvider{Python: python, Worker: script}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	qs, e := p.Qualities(ctx, "fixture")
	if e != nil || len(qs) != 2 {
		t.Fatal("quality probe")
	}
	var output bytes.Buffer
	start := time.Now()
	e = p.Stream(ctx, "fixture", "360p", true, &output)
	if e == nil || output.String() != "NORMALNORMAL" || time.Since(start) < 2*time.Second {
		t.Fatal("unbounded/rapid reacquisition or cancellation failure")
	}
	// A nonterminating worker must be stopped without waiting for its own timeout.
	if e := os.WriteFile(script, []byte("import time\ntime.sleep(30)\n"), 0600); e != nil {
		t.Fatal(e)
	}
	bounded, stop := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer stop()
	start = time.Now()
	_ = p.Stream(bounded, "fixture", "auto", true, io.Discard)
	if time.Since(start) > time.Second {
		t.Fatal("worker cancellation failed")
	}
}
