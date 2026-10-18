// Package config loads queue-service settings from a .env file with
// environment-variable overrides.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds queue-service settings.
type Config struct {
	Port                  int
	DataDir               string
	RepositoryType        string // "file" (default) or "sqlite"
	OutDir                string
	MaxDownloadWorkers    int
	MaxReconstructWorkers int
	ACUsername            string
	ACPassword            string
	DefaultResolution     string
	DefaultFPS            int
	DefaultPreset         string
	DefaultCRF            int
	ExportMP3             bool
	RetryCount            int
}

// defaults returns the default Config.
func defaults() Config {
	return Config{
		Port:                  8080,
		DataDir:               "./data",
		RepositoryType:        "file",
		OutDir:                "./recordings",
		MaxDownloadWorkers:    3,
		MaxReconstructWorkers: 1,
		DefaultResolution:     "720p",
		DefaultFPS:            25,
		DefaultPreset:         "veryfast",
		DefaultCRF:            23,
		ExportMP3:             true,
		RetryCount:            2,
	}
}

// Load reads path as a KEY=VALUE .env file (missing file -> defaults) and
// lets non-empty environment variables override file values.
func Load(path string) (Config, error) {
	c := defaults()
	vals := map[string]string{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return c, err
			}
		} else {
			vals = parse(b)
		}
	}
	// file values
	if err := apply(&c, vals); err != nil {
		return c, err
	}
	// env overrides (non-empty wins)
	env := map[string]string{}
	// env overrides (non-empty wins). Note: USERNAME/PASSWORD are file-only
	// fallbacks - on Windows USERNAME is always set (the login name), so
	// honoring it here would clobber AC_USERNAME from the file.
	for _, k := range []string{"PORT", "DATA_DIR", "REPOSITORY_TYPE", "OUT_DIR",
		"MAX_DOWNLOAD_WORKERS", "MAX_RECONSTRUCT_WORKERS",
		"AC_USERNAME", "AC_PASSWORD",
		"DEFAULT_RESOLUTION", "DEFAULT_FPS", "DEFAULT_PRESET",
		"DEFAULT_CRF", "EXPORT_MP3", "RETRY_COUNT"} {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env[k] = v
		}
	}
	if len(env) > 0 {
		if err := apply(&c, env); err != nil {
			return c, err
		}
	}
	// Fail loudly on a typo rather than silently falling back to the file store.
	switch c.RepositoryType {
	case "", "file":
		c.RepositoryType = "file"
	case "sqlite":
	default:
		return c, fmt.Errorf("REPOSITORY_TYPE must be \"file\" or \"sqlite\", got %q", c.RepositoryType)
	}
	return c, nil
}

func parse(b []byte) map[string]string {
	vals := map[string]string{}
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		k, v, ok := strings.Cut(ln, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(k), "export "))
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		vals[k] = v
	}
	return vals
}

func apply(c *Config, vals map[string]string) error {
	str := func(keys ...string) string {
		for _, k := range keys {
			if vals[k] != "" {
				return vals[k]
			}
		}
		return ""
	}
	num := func(key string, dst *int) error {
		if vals[key] == "" {
			return nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(vals[key]))
		if err != nil {
			return err
		}
		*dst = n
		return nil
	}
	if err := num("PORT", &c.Port); err != nil {
		return err
	}
	if v := str("DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := str("REPOSITORY_TYPE"); v != "" {
		c.RepositoryType = v
	}
	if v := str("OUT_DIR"); v != "" {
		c.OutDir = v
	}
	if err := num("MAX_DOWNLOAD_WORKERS", &c.MaxDownloadWorkers); err != nil {
		return err
	}
	if err := num("MAX_RECONSTRUCT_WORKERS", &c.MaxReconstructWorkers); err != nil {
		return err
	}
	if v := str("AC_USERNAME", "USERNAME"); v != "" {
		c.ACUsername = v
	}
	if v := str("AC_PASSWORD", "PASSWORD"); v != "" {
		c.ACPassword = v
	}
	if v := str("DEFAULT_RESOLUTION"); v != "" {
		c.DefaultResolution = v
	}
	if err := num("DEFAULT_FPS", &c.DefaultFPS); err != nil {
		return err
	}
	if v := str("DEFAULT_PRESET"); v != "" {
		c.DefaultPreset = v
	}
	if err := num("DEFAULT_CRF", &c.DefaultCRF); err != nil {
		return err
	}
	if vals["EXPORT_MP3"] != "" {
		b, err := strconv.ParseBool(strings.TrimSpace(vals["EXPORT_MP3"]))
		if err != nil {
			return err
		}
		c.ExportMP3 = b
	}
	if err := num("RETRY_COUNT", &c.RetryCount); err != nil {
		return err
	}
	return nil
}

// ResolutionDims maps DefaultResolution to width/height; unknown -> 720p.
func (c Config) ResolutionDims() (w, h int) {
	switch c.DefaultResolution {
	case "480p":
		return 854, 480
	case "1080p":
		return 1920, 1080
	default:
		return 1280, 720
	}
}
