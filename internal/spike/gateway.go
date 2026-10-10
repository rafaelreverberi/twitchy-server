// Package spike contains an OFFLINE ONLY feasibility experiment for path A.
// It is not imported by the server and is not a production HLS filter.
package spike

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var attrs = regexp.MustCompile(`([A-Z-]+)=(?:"([^"]*)"|([^,]*))`)

type period struct{ start, end time.Time }

// Rewrite evaluates the fixture subset of Twitch HLS, assigning an own sequence
// and discontinuity on recovery. Real refresh state/key/map/timeline handling is
// deliberately required before this path could ever replace the byte reader.
func Rewrite(manifest string, sequence int) (string, error) {
	var ranges []period
	lines := strings.Split(manifest, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "#EXT-X-MAP:") || strings.HasPrefix(line, "#EXT-X-PART:") {
			return "", errors.New("unsupported initialization or partial media")
		}
		if strings.HasPrefix(line, "#EXT-X-DATERANGE:") {
			a := map[string]string{}
			for _, v := range attrs.FindAllStringSubmatch(line, -1) {
				if v[2] != "" {
					a[v[1]] = v[2]
				} else {
					a[v[1]] = v[3]
				}
			}
			if a["CLASS"] == "twitch-stitched-ad" || strings.HasPrefix(a["ID"], "stitched-ad-") {
				start, e := time.Parse(time.RFC3339, a["START-DATE"])
				duration, err := strconv.ParseFloat(a["DURATION"], 64)
				if e != nil || err != nil {
					return "", errors.New("ambiguous ad range")
				}
				ranges = append(ranges, period{start, start.Add(time.Duration(duration * float64(time.Second)))})
			}
		}
	}
	out := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:" + strconv.Itoa(sequence) + "\n"
	var stamp time.Time
	inf := ""
	ad, paused, written := false, false, false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			var e error
			stamp, e = time.Parse(time.RFC3339, strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"))
			if e != nil {
				return "", e
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			inf = line
			ad = strings.HasSuffix(line, ",Amazon") || strings.Contains(line, ",Amazon|")
			for _, r := range ranges {
				if stamp.IsZero() || (!stamp.Before(r.start) && stamp.Before(r.end)) {
					ad = true
				}
			}
		case line != "" && !strings.HasPrefix(line, "#"):
			if inf == "" {
				return "", errors.New("missing duration")
			}
			if ad {
				paused = true
			} else {
				if paused && written {
					out += "#EXT-X-DISCONTINUITY\n"
				}
				out += inf + "\n" + line + "\n"
				written = true
				paused = false
			}
			inf = ""
		}
	}
	return out, nil
}

type Feed struct {
	Channel, Codec, Quality string
	Observed                time.Time
	Aligned                 bool
}

func ValidateBackup(primary, codec string, feed Feed, now time.Time) error {
	if feed.Channel != primary || feed.Codec != codec || now.Sub(feed.Observed) > 10*time.Second || feed.Observed.After(now) || !feed.Aligned {
		return errors.New("backup cannot safely join primary timeline")
	}
	return nil
}
