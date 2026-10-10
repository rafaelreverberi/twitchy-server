package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AdPolicy is independent of acquisition. No unverified backup is enabled.
type AdPolicy interface {
	Enabled() bool
	Mode() string
}
type FilterPause struct{ On bool }

func (p FilterPause) Enabled() bool { return p.On }
func (p FilterPause) Mode() string {
	if p.On {
		return "pause"
	}
	return "none"
}

// A future backup selector must attest same channel, freshness and codec/timeline
// compatibility before it can feed the remuxer. No raw playlist switching.
type BackupCandidate struct {
	Channel, Codec string
	ObservedAt     time.Time
}
type BackupSelector interface {
	Select(context.Context, string, string) (BackupCandidate, error)
}

type Session struct {
	ID, Owner, Dir string
	Expires, Last  time.Time
	cancel         context.CancelFunc
	done           chan struct{}
}
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	pending  map[string]int
	cfg      Config
	provider Provider
	policy   AdPolicy
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func NewManager(c Config, p Provider, policy AdPolicy) *SessionManager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &SessionManager{sessions: map[string]*Session{}, pending: map[string]int{}, cfg: c, provider: p, policy: policy, ctx: ctx, cancel: cancel}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				m.sweep(now)
			}
		}
	}()
	return m
}
func (m *SessionManager) reserve(owner string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	total, count := 0, m.pending[owner]
	for _, n := range m.pending {
		total += n
	}
	for _, s := range m.sessions {
		total++
		if s.Owner == owner {
			count++
		}
	}
	if total >= m.cfg.MaxSessions || count >= m.cfg.PerPrincipal {
		return false
	}
	m.pending[owner]++
	return true
}
func (m *SessionManager) unreserve(owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[owner]--
	if m.pending[owner] == 0 {
		delete(m.pending, owner)
	}
}
func randomID() string {
	var b [24]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func (m *SessionManager) start(ctx context.Context, owner, channel, quality string) (*Session, error) {
	id := randomID()
	dir := filepath.Join(m.cfg.MediaDir, id)
	if e := os.Mkdir(dir, 0700); e != nil {
		return nil, e
	}
	life, cancel := context.WithCancel(m.ctx)
	s := &Session{ID: id, Owner: owner, Dir: dir, Expires: time.Now().UTC().Add(m.cfg.Lifetime).Truncate(time.Second), Last: time.Now(), cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(s.done)
		defer os.RemoveAll(dir)
		defer cancel()
		// Copy only; TS demuxer adjusts discontinuous input timestamps. No upstream
		// URL is ever passed to FFmpeg, so its protocol access is pipe-only.
		cmd := exec.CommandContext(life, m.cfg.FFmpeg, "-hide_banner", "-loglevel", "error", "-fflags", "+genpts+discardcorrupt", "-dts_delta_threshold", "1", "-probesize", "262144", "-analyzeduration", "500000", "-protocol_whitelist", "pipe", "-f", "mpegts", "-i", "pipe:0", "-map", "0:v:0", "-map", "0:a:0?", "-c", "copy", "-bsf:a", "setts=ts='if(eq(N,0),TS,max(TS,PREV_OUTDTS+DURATION))'", "-avoid_negative_ts", "make_zero", "-f", "hls", "-hls_time", "2", "-hls_list_size", "10", "-hls_delete_threshold", "3", "-hls_flags", "delete_segments+temp_file+discont_start", "-hls_segment_filename", filepath.Join(dir, "seg%09d.ts"), filepath.Join(dir, "index.m3u8"))
		process(cmd)
		input, e := cmd.StdinPipe()
		if e != nil {
			return
		}
		if cmd.Start() != nil {
			input.Close()
			return
		}
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			_ = m.provider.Stream(life, channel, quality, m.policy.Enabled(), input)
			input.Close()
		}()
		_ = cmd.Wait()
		cancel()
		input.Close()
		<-finished
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.remove(id)
			return nil, ctx.Err()
		case <-s.done:
			m.remove(id)
			return nil, ErrProvider
		case <-ticker.C:
			b, e := os.ReadFile(filepath.Join(dir, "index.m3u8"))
			if e == nil && strings.Contains(string(b), "#EXTINF:") {
				return s, nil
			}
		}
	}
}
func (m *SessionManager) remove(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s != nil {
		s.cancel()
		<-s.done
	}
}
func (m *SessionManager) get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s != nil {
		if time.Now().After(s.Expires) {
			return nil
		}
		select {
		case <-s.done:
			return nil
		default:
		}
		s.Last = time.Now()
	}
	return s
}
func (m *SessionManager) owner(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[id]; s != nil {
		return s.Owner
	}
	return ""
}
func (m *SessionManager) sweep(now time.Time) {
	m.mu.Lock()
	var ids []string
	for id, s := range m.sessions {
		expired := now.After(s.Expires) || now.Sub(s.Last) > m.cfg.Idle
		select {
		case <-s.done:
			expired = true
		default:
		}
		var size int64
		_ = filepath.WalkDir(s.Dir, func(path string, d os.DirEntry, e error) error {
			if e == nil && !d.IsDir() {
				if info, e := d.Info(); e == nil {
					size += info.Size()
				}
			}
			return nil
		})
		if expired || size > m.cfg.MaxDisk {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.remove(id)
	}
}
func (m *SessionManager) Close() {
	m.cancel()
	m.mu.Lock()
	var ids []string
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.remove(id)
	}
	m.wg.Wait()
}
func (m *SessionManager) Prepare() error {
	if e := os.MkdirAll(m.cfg.MediaDir, 0700); e != nil {
		return e
	}
	entries, e := os.ReadDir(m.cfg.MediaDir)
	if e != nil {
		return e
	}
	for _, d := range entries {
		if d.IsDir() && len(d.Name()) == 48 && identifier.MatchString(d.Name()) {
			if e := os.RemoveAll(filepath.Join(m.cfg.MediaDir, d.Name())); e != nil {
				return fmt.Errorf("stale session cleanup failed")
			}
		}
	}
	return nil
}

var _ io.Writer = (*cappedBuffer)(nil)
