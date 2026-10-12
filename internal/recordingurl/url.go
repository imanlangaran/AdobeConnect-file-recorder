// Package recordingurl parses and validates Adobe Connect recording URLs.
package recordingurl

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// ErrInvalidURL indicates the input is not a usable recording URL.
var ErrInvalidURL = errors.New("invalid recording URL")

// RecordingURL is a parsed, validated recording reference.
type RecordingURL struct {
	Scheme  string // http or https
	Host    string // host[:port]
	ID      string // recording id, e.g. "p15rdeovw4ge"
	BaseURL string // scheme://host/<id>
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Parse accepts "https://host/id/", "host/id", etc. and returns a RecordingURL.
func Parse(raw string) (RecordingURL, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if raw == "" {
		return RecordingURL{}, fmt.Errorf("%w: empty", ErrInvalidURL)
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return RecordingURL{}, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	if u.Host == "" || u.User != nil {
		return RecordingURL{}, fmt.Errorf("%w: host missing or userinfo present", ErrInvalidURL)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || !idRe.MatchString(parts[0]) {
		return RecordingURL{}, fmt.Errorf("%w: recording id missing", ErrInvalidURL)
	}
	id := parts[0]
	return RecordingURL{
		Scheme:  u.Scheme,
		Host:    u.Host,
		ID:      id,
		BaseURL: fmt.Sprintf("%s://%s/%s", u.Scheme, u.Host, id),
	}, nil
}

// PageURL is the recording page (with the original query, e.g. ?session=...).
func (r RecordingURL) PageURL() string { return r.BaseURL + "/" }

// ZipURL is the raw recording archive endpoint, verified against webinar5.um.ac.ir.
func (r RecordingURL) ZipURL() string {
	return fmt.Sprintf("%s/output/%s.zip?download=zip", r.BaseURL, r.ID)
}
