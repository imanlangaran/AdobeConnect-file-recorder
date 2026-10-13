package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseResolution(t *testing.T) {
	for _, want := range []resOption{
		{"480p", 854, 480},
		{"720p", 1280, 720},
		{"1080p", 1920, 1080},
	} {
		got, err := parseResolution(want.name)
		if err != nil {
			t.Fatalf("parseResolution(%q): %v", want.name, err)
		}
		if got != want {
			t.Errorf("parseResolution(%q) = %+v, want %+v", want.name, got, want)
		}
		if got.w%2 != 0 || got.h%2 != 0 {
			t.Errorf("%q dims must be even for yuv420p: %dx%d", got.name, got.w, got.h)
		}
	}
}

func TestParseResolutionRejectsInvalid(t *testing.T) {
	for _, bad := range []string{"1080i", "4k", "1366x768", "480", ""} {
		_, err := parseResolution(bad)
		if err == nil {
			t.Errorf("parseResolution(%q) accepted invalid input", bad)
			continue
		}
		// the error must teach the valid options
		for _, opt := range []string{"480p", "720p", "1080p"} {
			if !strings.Contains(err.Error(), opt) {
				t.Errorf("error for %q must list %q: %v", bad, opt, err)
			}
		}
	}
}

func TestReadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	os.WriteFile(path, []byte(
		"# credentials\n\nAC_USERNAME=alice\nAC_PASSWORD=\"s3cr3t\"\n"), 0o600)
	u, p, err := readEnvFile(path)
	if err != nil || u != "alice" || p != "s3cr3t" {
		t.Errorf("readEnvFile = %q, %q, %v", u, p, err)
	}

	// plain USERNAME/PASSWORD keys, export prefix, single quotes
	os.WriteFile(path, []byte("export USERNAME=bob\nPASSWORD='pw'\n"), 0o600)
	if u, p, err = readEnvFile(path); err != nil || u != "bob" || p != "pw" {
		t.Errorf("fallback keys: %q, %q, %v", u, p, err)
	}

	// missing file → no auth, no error
	if u, p, err = readEnvFile(filepath.Join(dir, "nope.env")); err != nil || u != "" || p != "" {
		t.Errorf("missing file: %q, %q, %v", u, p, err)
	}

	// file without credentials → clear error
	os.WriteFile(path, []byte("UNRELATED=1\n"), 0o600)
	if _, _, err = readEnvFile(path); err == nil {
		t.Error("missing credentials must be an error")
	}
}

func TestNewProgressLabels(t *testing.T) {
	// the printed stage name must come from the callback, not be hardcoded
	for _, stage := range []string{"download", "extract", "reconstruct"} {
		var buf bytes.Buffer
		p := newProgress(&buf)
		p(stage, 100, 200)
		if !strings.Contains(buf.String(), stage+" ") {
			t.Errorf("progress for stage %q printed %q", stage, buf.String())
		}
		if !strings.Contains(buf.String(), "elapsed]") {
			t.Errorf("progress for stage %q misses the elapsed-time suffix: %q", stage, buf.String())
		}
	}
}
