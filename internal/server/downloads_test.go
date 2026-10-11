package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func downloadFixture(t *testing.T, audio bool) []byte {
	t.Helper()
	args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25"}
	if audio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-ac", "2", "-c:a", "aac")
	}
	args = append(args, "-t", "6", "-c:v", "libx264", "-threads", "1", "-g", "50", "-f", "mpegts", "pipe:1")
	media, err := exec.Command("ffmpeg", args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return media
}

func createDownload(t *testing.T, s *Server, kind string) string {
	t.Helper()
	id := "example"
	if kind == "vod" {
		id = "123"
	}
	w := request(t, s, "POST", "/v1/downloads", "a", fmt.Sprintf(`{"content_type":%q,"content_id":%q}`, kind, id))
	var job downloadJob
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil || w.Code != 202 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	return "/v1/downloads/" + job.ID
}

// This exercises the HTTP download handler and its actual FFmpeg command, not a
// duplicate remux command. Run in the smoke image too, using Debian's FFmpeg.
func TestDownloadAACConfigurationAndTimeline(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " missing")
		}
	}
	for _, audio := range []bool{true, false} {
		media := downloadFixture(t, audio)
		for _, kind := range []string{"clip", "vod"} {
			t.Run(fmt.Sprintf("%s/audio=%t", kind, audio), func(t *testing.T) {
				c := testConfig(t)
				s, err := New(c, recordedFixture{stubProvider: stubProvider{ready: true}, media: media})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				route := createDownload(t, s, kind)
				response := request(t, s, "GET", route+"/file", "a", "")
				if response.Code != 200 || response.Header().Get("Content-Type") != "video/mp4" || response.Header().Get("Content-Length") != "" {
					t.Fatal("stream response", response.Code)
				}
				file := filepath.Join(t.TempDir(), "client.mp4")
				if err := os.WriteFile(file, response.Body.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				verifyDownloadMedia(t, file, audio)
				// Optional artifacts for independent macOS AVFoundation verification.
				if dir := os.Getenv("DOWNLOAD_TEST_ARTIFACTS"); dir != "" {
					if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-audio-%t.mp4", kind, audio)), response.Body.Bytes(), 0600); err != nil {
						t.Fatal(err)
					}
				}
				var job downloadJob
				if err := json.Unmarshal(request(t, s, "GET", route, "a", "").Body.Bytes(), &job); err != nil {
					t.Fatal(err)
				}
				if job.Status != "completed" || job.Progress != 1 || job.Bytes != int64(response.Body.Len()) {
					t.Fatal("completion", job)
				}
				files, err := os.ReadDir(c.MediaDir)
				if err != nil || len(files) != 0 {
					t.Fatal("server archive retained", err)
				}
			})
		}
	}
}

func verifyDownloadMedia(t *testing.T, file string, audio bool) {
	t.Helper()
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-show_data", "-show_packets", "-of", "json", file).Output()
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Streams []struct {
			Index         int
			CodecType     string `json:"codec_type"`
			CodecName     string `json:"codec_name"`
			Profile       string
			SampleRate    string `json:"sample_rate"`
			Channels      int
			ExtradataSize int `json:"extradata_size"`
			Extradata     string
		}
		Packets []struct {
			Index    int    `json:"stream_index"`
			PTS      string `json:"pts_time"`
			DTS      string `json:"dts_time"`
			Duration string `json:"duration_time"`
		}
	}
	if err := json.Unmarshal(probe, &result); err != nil {
		t.Fatal(err)
	}
	expected := 1
	if audio {
		expected = 2
	}
	if len(result.Streams) != expected {
		t.Fatal("track count", len(result.Streams))
	}
	starts, ends := map[int]float64{}, map[int]float64{}
	// FFprobe 5.1 omits duration_time on some fragmented AAC packets. Obtain
	// those durations from decoded frame sample counts rather than assuming them.
	audioDurations := map[string]float64{}
	if audio {
		frames, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0", "-show_frames", "-show_entries", "frame=best_effort_timestamp_time,nb_samples", "-of", "json", file).Output()
		if err != nil {
			t.Fatal("audio frame probe", err)
		}
		var decoded struct {
			Frames []struct {
				PTS     string `json:"best_effort_timestamp_time"`
				Samples int    `json:"nb_samples"`
			}
		}
		if err := json.Unmarshal(frames, &decoded); err != nil {
			t.Fatal(err)
		}
		for _, frame := range decoded.Frames {
			audioDurations[frame.PTS] = float64(frame.Samples) / 48000
		}
	}
	for _, stream := range result.Streams {
		if stream.CodecType == "audio" && (stream.CodecName != "aac" || stream.Profile != "LC" || stream.SampleRate != "48000" || stream.Channels != 2 || stream.ExtradataSize != 2 || !strings.Contains(stream.Extradata, "1190")) {
			t.Fatalf("missing/invalid AAC-LC 48kHz stereo AudioSpecificConfig: %+v", stream)
		}
		previous := math.Inf(-1)
		start, end := math.Inf(1), math.Inf(-1)
		for _, packet := range result.Packets {
			if packet.Index != stream.Index {
				continue
			}
			pts, e1 := strconv.ParseFloat(packet.PTS, 64)
			dts, e2 := strconv.ParseFloat(packet.DTS, 64)
			duration, e3 := strconv.ParseFloat(packet.Duration, 64)
			if stream.CodecType == "audio" && packet.Duration == "" {
				duration = audioDurations[packet.PTS]
				e3 = nil
			}
			if e1 != nil || e2 != nil || e3 != nil || duration <= 0 || dts <= previous || dts < -0.001 {
				t.Fatalf("invalid packet timing: %+v after %f", packet, previous)
			}
			previous = dts
			start = math.Min(start, pts)
			end = math.Max(end, pts+duration)
		}
		if start < -0.001 || end-start < 5.9 || end-start > 6.2 {
			t.Fatalf("incomplete %s timeline: %f..%f", stream.CodecType, start, end)
		}
		starts[stream.Index], ends[stream.Index] = start, end
		t.Logf("%s: start=%.3fs end=%.3fs config=%d bytes", stream.CodecType, start, end, stream.ExtradataSize)
	}
	if audio && (math.Abs(starts[0]-starts[1]) > 0.1 || math.Abs(ends[0]-ends[1]) > 0.1) {
		t.Fatal("A/V desynchronization", starts, ends)
	}
	// Force decoding of every video frame (including the last fragment).
	decoded, err := exec.Command("ffmpeg", "-v", "error", "-xerror", "-i", file, "-map", "0:v:0", "-f", "framemd5", "pipe:1").Output()
	if err != nil {
		t.Fatal("video decode", err)
	}
	frames := 0
	for _, line := range strings.Split(string(decoded), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			frames++
		}
	}
	if frames != 150 {
		t.Fatal("decoded video frames", frames)
	}
	if !audio {
		return
	}
	pcm, err := exec.Command("ffmpeg", "-v", "error", "-xerror", "-i", file, "-map", "0:a:0", "-ac", "1", "-ar", "48000", "-f", "s16le", "pipe:1").Output()
	if err != nil {
		t.Fatal("audio decode", err)
	}
	samples := len(pcm) / 2
	if samples < 48000*59/10 || samples > 48000*62/10 {
		t.Fatal("incomplete decoded audio", samples)
	}
	// Check each second, including the tail: merely listing a track is insufficient.
	for start := 0; start < 6*48000; start += 48000 {
		end := min(start+48000, samples)
		energy := 0.0
		for i := start; i < end; i++ {
			value := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
			energy += value * value
		}
		rms := math.Sqrt(energy / float64(end-start))
		if rms < 0.02 || rms > 0.2 {
			t.Fatalf("test tone RMS at second %d: %f", start/48000, rms)
		}
		t.Logf("audio second %d RMS=%.4f", start/48000, rms)
	}
}

type controlledDownload struct {
	recordedFixture
	media   []byte
	started chan struct{}
	hold    bool
}

func (p controlledDownload) Stream(ctx context.Context, _ string, _ string, _ bool, w io.Writer) error {
	close(p.started)
	if len(p.media) > 0 {
		if _, err := w.Write(p.media); err != nil {
			return err
		}
	}
	if p.hold {
		<-ctx.Done()
		return ctx.Err()
	}
	return ErrUnavailable
}

func TestDownloadFailureAndCancellation(t *testing.T) {
	media := downloadFixture(t, true)
	for _, mode := range []string{"early-error", "late-error", "cancel", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			fixture := controlledDownload{recordedFixture: recordedFixture{stubProvider: stubProvider{ready: true}}, started: started, hold: mode == "cancel" || mode == "disconnect"}
			if mode != "early-error" {
				fixture.media = media
			}
			s, err := New(testConfig(t), fixture)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			route := createDownload(t, s, "vod")
			host := httptest.NewServer(s)
			defer host.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", host.URL+route+"/file", nil)
			req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 43))
			done := make(chan error, 1)
			headers := make(chan int, 1)
			go func() {
				response, err := host.Client().Do(req)
				if err == nil {
					defer response.Body.Close()
					headers <- response.StatusCode
					_, err = io.Copy(io.Discard, response.Body)
					if mode == "early-error" && response.StatusCode != 503 {
						err = fmt.Errorf("early failure status: %d", response.StatusCode)
					}
				}
				done <- err
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("provider did not start")
			}
			if fixture.hold {
				select {
				case status := <-headers:
					if status != 200 {
						t.Fatal("transfer did not start", status)
					}
				case <-ctx.Done():
					t.Fatal("no streamed response before cancellation")
				}
			}
			if mode == "cancel" {
				if w := request(t, s, "DELETE", route, "a", ""); w.Code != 204 {
					t.Fatal(w.Code)
				}
			}
			if mode == "disconnect" {
				cancel()
			}
			select {
			case err := <-done:
				if mode == "early-error" && err != nil {
					t.Fatal(err)
				}
				if mode != "early-error" && err == nil {
					t.Fatal("missing transfer failure")
				}
			case <-ctx.Done():
				if mode != "disconnect" {
					t.Fatal("transfer did not stop")
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				var job downloadJob
				json.Unmarshal(request(t, s, "GET", route, "a", "").Body.Bytes(), &job)
				expected := "failed"
				if mode == "cancel" {
					expected = "cancelled"
				}
				if job.Status == expected {
					if expected == "failed" && job.Error == nil {
						t.Fatal("missing retryable error")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("terminal state", job.Status)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
