// Package assets extracts a recording archive (ZIP) and its sidecar data
// (chat, attendees, metadata).
package assets

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Limits guard against zip bombs and path traversal.
const (
	maxEntries     = 10_000
	maxTotalBytes  = 100 << 30 // 100 GiB uncompressed
	maxRatioFactor = 200
)

// ErrBadArchive indicates the file is not a usable recording ZIP.
var ErrBadArchive = errors.New("not a valid recording archive")

// Extract unpacks src ZIP into dir. Members are placed flat (recording
// archives use no meaningful directory structure); path traversal and
// decompression bombs are rejected. progress (optional, may be nil) is
// called with (done, total) uncompressed bytes, throttled to ~10/s.
func Extract(src, dir string, progress func(done, total int64)) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	defer zr.Close()

	if len(zr.File) == 0 || len(zr.File) > maxEntries {
		return fmt.Errorf("%w: %d members", ErrBadArchive, len(zr.File))
	}
	var totalUncompressed int64
	for _, f := range zr.File {
		totalUncompressed += int64(f.UncompressedSize64)
		if totalUncompressed > maxTotalBytes {
			return fmt.Errorf("%w: uncompressed size exceeds limit", ErrBadArchive)
		}
	}
	if len(zr.File) > 10 && totalUncompressed > int64(zr.File[0].CompressedSize64+1)*maxRatioFactor*int64(len(zr.File)) {
		return fmt.Errorf("%w: suspicious compression ratio", ErrBadArchive)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	cr := &countReader{total: totalUncompressed, fn: progress}
	for _, f := range zr.File {
		name := filepath.Base(filepath.FromSlash(f.Name))
		if name == "." || name == string(filepath.Separator) || name == "" {
			return fmt.Errorf("%w: bad member name %q", ErrBadArchive, f.Name)
		}
		dst := filepath.Join(dir, name)
		if !strings.HasPrefix(dst, filepath.Clean(dir)+string(filepath.Separator)) {
			return fmt.Errorf("%w: path escape %q", ErrBadArchive, f.Name)
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if err := extractFile(f, dst, cr); err != nil {
			return err
		}
	}
	if progress != nil {
		progress(totalUncompressed, totalUncompressed)
	}
	return nil
}

// countReader counts bytes through it and reports throttled progress (~10/s).
// One instance is reused across archive members so `done` accumulates.
type countReader struct {
	r     io.Reader
	done  int64
	total int64
	fn    func(done, total int64)
	last  time.Time
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.done += int64(n)
		if c.fn != nil && time.Since(c.last) > 100*time.Millisecond {
			c.last = time.Now()
			c.fn(c.done, c.total)
		}
	}
	return n, err
}

func extractFile(f *zip.File, dst string, cr *countReader) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	// Limit each member to the bomb cap so a single huge file cannot fill the disk.
	cr.r = io.LimitReader(rc, maxTotalBytes)
	_, err = io.Copy(out, cr)
	return err
}

// Find returns the path of a member by name (flat layout), or "" if absent.
func Find(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := fs.Stat(os.DirFS(dir), name); err == nil {
		return p
	}
	return ""
}

// FindPrefix returns the first file in dir whose name starts with prefix.
func FindPrefix(dir, prefix string) string {
	m, err := filepath.Glob(filepath.Join(dir, prefix+"*"))
	if err != nil || len(m) == 0 {
		return ""
	}
	return m[0]
}
