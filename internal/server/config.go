package server

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen, BaseURL, Prefix, Python, Worker, FFmpeg, MediaDir string
	Tokens                                                    map[string]string
	Key                                                       []byte
	Adblock, Development                                      bool
	MaxSessions, PerPrincipal                                 int
	Lifetime, Idle, Startup                                   time.Duration
	MaxDisk                                                   int64
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
func LoadConfig() (Config, error) {
	c := Config{Listen: env("LISTEN_ADDR", ":8080"), BaseURL: env("PUBLIC_BASE_URL", "http://localhost:8080"), Python: env("PYTHON", "python3"), Worker: env("WORKER_PATH", "worker/reader.py"), FFmpeg: env("FFMPEG", "ffmpeg"), MediaDir: env("MEDIA_DIR", "state/media"), Adblock: env("ADBLOCK", "on") == "on", Development: env("ALLOW_HTTP", "false") == "true"}
	if v := env("ADBLOCK", "on"); v != "on" && v != "off" {
		return c, errors.New("ADBLOCK must be on or off")
	}
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && c.Development)) {
		return c, errors.New("PUBLIC_BASE_URL requires HTTPS (or explicit ALLOW_HTTP development mode)")
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	c.Prefix = strings.TrimRight(u.Path, "/")
	if strings.Contains(c.Prefix, "..") || u.RawPath != "" {
		return c, errors.New("invalid base path")
	}
	b, e := os.ReadFile(env("TOKEN_FILE", "secrets/tokens.json"))
	if e != nil {
		return c, errors.New("cannot read TOKEN_FILE")
	}
	if json.Unmarshal(b, &c.Tokens) != nil || len(c.Tokens) == 0 || len(c.Tokens) > 64 {
		return c, errors.New("invalid principal token file")
	}
	seen := map[string]bool{}
	for p, t := range c.Tokens {
		if !identifier.MatchString(p) || len(t) < 43 || seen[t] {
			return c, errors.New("unique high entropy tokens and valid principals required")
		}
		seen[t] = true
	}
	c.Key, e = os.ReadFile(env("SIGNING_KEY_FILE", "secrets/signing.key"))
	if e != nil || len(c.Key) < 32 {
		return c, errors.New("32 byte signing key required")
	}
	for _, s := range []struct {
		k   string
		d   int
		out *int
	}{{"MAX_SESSIONS", 8, &c.MaxSessions}, {"SESSIONS_PER_PRINCIPAL", 2, &c.PerPrincipal}} {
		n, e := strconv.Atoi(env(s.k, strconv.Itoa(s.d)))
		if e != nil || n < 1 || n > 64 {
			return c, errors.New("invalid session limit")
		}
		*s.out = n
	}
	for _, s := range []struct {
		k, d string
		out  *time.Duration
	}{{"SESSION_LIFETIME", "12h", &c.Lifetime}, {"SESSION_IDLE_TTL", "2m", &c.Idle}, {"SESSION_STARTUP_TIMEOUT", "90s", &c.Startup}} {
		v, e := time.ParseDuration(env(s.k, s.d))
		if e != nil || v < time.Second || v > 24*time.Hour {
			return c, errors.New("invalid timeout")
		}
		*s.out = v
	}
	c.MaxDisk, e = strconv.ParseInt(env("SESSION_MAX_DISK_BYTES", "268435456"), 10, 64)
	if e != nil || c.MaxDisk < 1048576 {
		return c, errors.New("invalid disk limit")
	}
	return c, nil
}
