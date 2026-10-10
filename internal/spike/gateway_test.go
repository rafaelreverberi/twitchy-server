package spike

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestGatewaySubsetAndUnsafeBackup(t *testing.T) {
	b, e := os.ReadFile("../../tests/fixtures/twitch-ad-markers.m3u8")
	if e != nil {
		t.Fatal(e)
	}
	out, e := Rewrite(string(b), 100)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(out, "1.ts") || !strings.Contains(out, "0.ts") || !strings.Contains(out, "2.ts") || !strings.Contains(out, "#EXT-X-DISCONTINUITY") {
		t.Fatal(out)
	}
	if _, e = Rewrite(string(b)+"#EXT-X-MAP:URI=init.mp4\n", 100); e == nil {
		t.Fatal("unsafe init accepted")
	}
	now := time.Now()
	good := Feed{"example", "h264/aac", "360p", now, true}
	if ValidateBackup("example", "h264/aac", good, now) != nil {
		t.Fatal("healthy aligned fixture rejected")
	}
	for _, bad := range []Feed{{"other", "h264/aac", "360p", now, true}, {"example", "hevc/aac", "360p", now, true}, {"example", "h264/aac", "360p", now.Add(-time.Minute), true}, {"example", "h264/aac", "360p", now, false}} {
		if ValidateBackup("example", "h264/aac", bad, now) == nil {
			t.Fatal("broken backup accepted")
		}
	}
}
