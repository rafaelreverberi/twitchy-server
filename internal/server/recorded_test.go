package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordedFixture struct {
	stubProvider
	media []byte
}

func (p recordedFixture) Recorded() bool { return true }
func (p recordedFixture) Plan(context.Context, string, string, bool) (RecordedPlan, error) {
	return RecordedPlan{Playlist: "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MAP:URI=\"init000000000.mp4\"\n#EXTINF:2,\nseg000000001.ts\n#EXT-X-ENDLIST\n", Resources: map[string]string{"init000000000.mp4": "https://cdn.twitchcdn.net/init.mp4", "seg000000001.ts": "https://cdn.twitchcdn.net/video.ts"}, Duration: 2}, nil
}
func (p recordedFixture) Stream(ctx context.Context, _ string, _ string, _ bool, w io.Writer) error {
	_, err := w.Write(p.media)
	return err
}
func TestRecordedFullTimelineSignedAndOwned(t *testing.T) {
	c := testConfig(t)
	s, err := New(c, recordedFixture{stubProvider: stubProvider{ready: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := request(t, s, "POST", "/v1/playback/sessions", "a", `{"content_type":"vod","content_id":"123","requested_quality":"auto"}`)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var session struct {
		ID  string `json:"session_id"`
		URL string `json:"hls_url"`
	}
	json.Unmarshal(w.Body.Bytes(), &session)
	master := request(t, s, "GET", session.URL, "", "")
	var variant string
	for _, line := range strings.Split(master.Body.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			variant = line
		}
	}
	media := request(t, s, "GET", variant, "", "")
	if media.Code != 200 || !strings.Contains(media.Body.String(), "#EXT-X-ENDLIST") || strings.Contains(media.Body.String(), "cdn.twitchcdn.net") || !strings.Contains(media.Body.String(), "sig=") {
		t.Fatal("full private signed timeline missing", media.Code)
	}
	if request(t, s, "DELETE", "/v1/playback/sessions/"+session.ID, "b", "").Code != 403 {
		t.Fatal("ownership")
	}
	if request(t, s, "DELETE", "/v1/playback/sessions/"+session.ID, "a", "").Code != 204 {
		t.Fatal("deletion")
	}
	if request(t, s, "GET", variant, "", "").Code == 200 {
		t.Fatal("revocation")
	}
}
func TestClientDownloadStreamsMP4WithoutServerFile(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg missing")
	}
	media, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=160x90:r=25:d=2", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=2", "-c:a", "aac", "-c:v", "libx264", "-threads", "1", "-f", "mpegts", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig(t)
	s, err := New(c, recordedFixture{stubProvider: stubProvider{ready: true}, media: media})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := request(t, s, "POST", "/v1/downloads", "a", `{"content_type":"clip","content_id":"example","requested_quality":"auto"}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var j downloadJob
	json.Unmarshal(w.Body.Bytes(), &j)
	if request(t, s, "GET", "/v1/downloads/"+j.ID+"/file", "b", "").Code != 403 {
		t.Fatal("ownership")
	}
	file := request(t, s, "GET", "/v1/downloads/"+j.ID+"/file", "a", "")
	if file.Code != 200 || file.Header().Get("Content-Type") != "video/mp4" || !strings.Contains(file.Body.String()[:min(100, file.Body.Len())], "ftyp") {
		t.Fatal("MP4 stream failed", file.Code)
	}
	destination := filepath.Join(t.TempDir(), "client.mp4")
	os.WriteFile(destination, file.Body.Bytes(), 0600)
	if err = exec.Command("ffmpeg", "-v", "error", "-i", destination, "-f", "null", "-").Run(); err != nil {
		t.Fatal("offline decoding failed", err)
	}
	paths, _ := os.ReadDir(c.MediaDir)
	if len(paths) != 0 {
		t.Fatal("server retained download media")
	}
	state := request(t, s, "GET", "/v1/downloads/"+j.ID, "a", "")
	json.Unmarshal(state.Body.Bytes(), &j)
	if j.Status != "completed" || j.Bytes == 0 {
		t.Fatal("completion state")
	}
	request(t, s, "DELETE", "/v1/downloads/"+j.ID, "a", "")
	if request(t, s, "GET", "/v1/downloads/"+j.ID+"/file", "a", "").Code != http.StatusConflict {
		t.Fatal("cancelled transfer still available")
	}
}
func TestRecordedDestinations(t *testing.T) {
	for _, raw := range []string{"http://cdn.ttvnw.net/a", "https://cdn.ttvnw.net.evil.test/a", "https://user:pass@cdn.ttvnw.net/a", "https://cdn.ttvnw.net:444/a", "https://127.0.0.1/a"} {
		if allowedUpstream(raw) {
			t.Fatal(raw)
		}
	}
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "100.64.0.1", "192.0.2.1", "2001:db8::1", "::1"} {
		if publicAddress(net.ParseIP(raw)) {
			t.Fatal(raw)
		}
	}
	if !allowedUpstream("https://d3fi1amfgojobc.cloudfront.net/a") || !publicAddress(net.ParseIP("1.1.1.1")) {
		t.Fatal("public CDN denied")
	}
}

type waitingRecorded struct {
	recordedFixture
	started chan struct{}
}

func (p waitingRecorded) Stream(ctx context.Context, _ string, _ string, _ bool, _ io.Writer) error {
	close(p.started)
	<-ctx.Done()
	return ctx.Err()
}
func TestRecordedShutdownCancelsActiveTransfer(t *testing.T) {
	c := testConfig(t)
	started := make(chan struct{})
	s, err := New(c, waitingRecorded{recordedFixture: recordedFixture{stubProvider: stubProvider{ready: true}}, started: started})
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, s, "POST", "/v1/downloads", "a", `{"content_type":"vod","content_id":"123"}`)
	var job downloadJob
	json.Unmarshal(w.Body.Bytes(), &job)
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	finished := make(chan struct{})
	go func() { defer close(finished); request(t, s, "GET", "/v1/downloads/"+job.ID+"/file", "a", "") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		s.Close()
		t.Fatal("transfer never started")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel transfer")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish")
	}
}

type delayedRecorded struct{ recordedFixture }

func (p delayedRecorded) Stream(ctx context.Context, _ string, _ string, _ bool, w io.Writer) error {
	select {
	case <-time.After(250 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	_, err := w.Write(p.media)
	return err
}
func TestArchiveTransferOverridesShortHTTPWriteDeadline(t *testing.T) {
	media, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=160x90:r=25:d=2", "-c:v", "libx264", "-threads", "1", "-f", "mpegts", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(testConfig(t), delayedRecorded{recordedFixture{stubProvider: stubProvider{ready: true}, media: media}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	create := request(t, s, "POST", "/v1/downloads", "a", `{"content_type":"vod","content_id":"123"}`)
	var job downloadJob
	json.Unmarshal(create.Body.Bytes(), &job)
	host := httptest.NewUnstartedServer(s)
	host.Config.WriteTimeout = 50 * time.Millisecond
	host.Start()
	defer host.Close()
	req, _ := http.NewRequest("GET", host.URL+"/v1/downloads/"+job.ID+"/file", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 43))
	response, err := host.Client().Do(req)
	if err != nil {
		t.Fatal("archive failed after short write deadline", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || len(body) < 1000 {
		t.Fatal("incomplete archive", response.StatusCode, len(body), err)
	}
}
