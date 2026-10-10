package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

var ErrUnavailable = errors.New("content unavailable")
var ErrProvider = errors.New("provider unavailable")

type UpstreamLimit struct{ After int }

func (e UpstreamLimit) Error() string { return "upstream rate limited" }

type Provider interface {
	Ready(context.Context) bool
	Qualities(context.Context, string) ([]string, error)
	Stream(context.Context, string, string, bool, io.Writer) error
	Live() bool
}
type StreamlinkProvider struct{ Python, Worker string }

func (p StreamlinkProvider) Live() bool { return true }
func (p StreamlinkProvider) Ready(ctx context.Context) bool {
	return exec.CommandContext(ctx, p.Python, p.Worker, "check").Run() == nil
}
func (p StreamlinkProvider) Qualities(ctx context.Context, channel string) ([]string, error) {
	cmd := exec.CommandContext(ctx, p.Python, p.Worker, "probe", "--channel", channel)
	var out cappedBuffer
	cmd.Stdout = &out
	_ = cmd.Run()
	var data struct {
		Qualities  []string `json:"qualities"`
		Error      string   `json:"error"`
		RetryAfter int      `json:"retry_after"`
	}
	if json.Unmarshal(out.data, &data) != nil {
		return nil, ErrProvider
	}
	if data.Error == "rate_limited" {
		after := data.RetryAfter
		if after < 1 || after > 300 {
			after = 30
		}
		return nil, UpstreamLimit{After: after}
	}
	if data.Error == "content_unavailable" {
		return nil, ErrUnavailable
	}
	if len(data.Qualities) < 2 || len(data.Qualities) > 40 {
		return nil, ErrProvider
	}
	return data.Qualities, nil
}

type cappedBuffer struct{ data []byte }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 65536 {
		return 0, errors.New("output limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
func process(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
}
func (p StreamlinkProvider) Stream(ctx context.Context, channel, quality string, ads bool, w io.Writer) error {
	mode := "off"
	if ads {
		mode = "on"
	}
	// Reacquire Twitch token/playlist after EOF/failure. Bounded attempts, fixed
	// quality, same continuous remuxer; no rapid mutation or reload loop.
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		cmd := exec.CommandContext(ctx, p.Python, p.Worker, "run", "--channel", channel, "--quality", quality, "--adblock", mode)
		process(cmd)
		cmd.Stdout = w
		cmd.Stderr = &stateEvents{}
		_ = cmd.Run()
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return ErrProvider
}

// FixtureProvider is explicitly injected by tests, never selectable from HTTP
// requests or environment in a release binary. It does not advertise live.
type FixtureProvider struct{ File string }

func (p FixtureProvider) Live() bool                 { return false }
func (p FixtureProvider) Ready(context.Context) bool { _, e := os.Stat(p.File); return e == nil }
func (p FixtureProvider) Qualities(context.Context, string) ([]string, error) {
	return []string{"auto", "360p"}, nil
}
func (p FixtureProvider) Stream(ctx context.Context, _ string, _ string, _ bool, w io.Writer) error {
	for {
		f, e := os.Open(filepath.Clean(p.File))
		if e != nil {
			return e
		}
		_, e = io.Copy(w, f)
		f.Close()
		if e != nil {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Only our private worker's two whitelisted states enter logs. Raw stderr,
// upstream URLs and channel/principal/session identifiers are never logged.
type stateEvents struct{ buffer []byte }

func (s *stateEvents) Write(p []byte) (int, error) {
	n := len(p)
	if len(s.buffer)+n > 4096 {
		s.buffer = nil
		return n, nil
	}
	s.buffer = append(s.buffer, p...)
	for {
		idx := bytes.IndexByte(s.buffer, '\n')
		if idx < 0 {
			break
		}
		var event struct {
			State string `json:"state"`
		}
		if json.Unmarshal(s.buffer[:idx], &event) == nil && (event.State == "playing" || event.State == "ad_break") {
			slog.Info("media state", "state", event.State)
		}
		s.buffer = s.buffer[idx+1:]
	}
	return n, nil
}
