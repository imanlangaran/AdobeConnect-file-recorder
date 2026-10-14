package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsOnMissingFile(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "no-such.env"))
	if err != nil {
		t.Fatalf("Load missing file: %v", err)
	}
	if c.Port != 8080 || c.DataDir != "./data" || c.OutDir != "./recordings" || c.MaxWorkers != 2 {
		t.Fatalf("bad defaults: %+v", c)
	}
	if w, h := c.ResolutionDims(); w != 1280 || h != 720 {
		t.Fatalf("bad default dims: %dx%d", w, h)
	}
	if c.ExportMP3 != true || c.RetryCount != 2 {
		t.Fatalf("bad defaults: %+v", c)
	}
}

func TestParseSampleFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	body := "# comment\nexport PORT=9090\nDATA_DIR=/tmp/data\nOUT_DIR=/tmp/rec\nMAX_WORKERS=4\n" +
		"AC_USERNAME='u@x.com'\nAC_PASSWORD=\"pw\"\nDEFAULT_RESOLUTION=1080p\nDEFAULT_FPS=30\n" +
		"DEFAULT_PRESET=fast\nDEFAULT_CRF=20\nEXPORT_MP3=false\nRETRY_COUNT=5\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != 9090 || c.MaxWorkers != 4 || c.ACUsername != "u@x.com" || c.ACPassword != "pw" {
		t.Fatalf("bad parse: %+v", c)
	}
	if w, h := c.ResolutionDims(); w != 1920 || h != 1080 {
		t.Fatalf("bad dims: %dx%d", w, h)
	}
	if c.ExportMP3 != false || c.RetryCount != 5 || c.DefaultFPS != 30 || c.DefaultCRF != 20 {
		t.Fatalf("bad parse: %+v", c)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte("PORT=9090\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORT", "1234")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != 1234 {
		t.Fatalf("env override failed: %+v", c)
	}
}
