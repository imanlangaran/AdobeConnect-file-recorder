// Package download streams recording files over HTTP with session auth,
// retries, progress reporting and format validation.
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ErrAuth indicates the server rejected the session (401/403 or login page).
var ErrAuth = errors.New("authentication rejected (session cookie missing or expired)")

// ErrNoContent indicates an empty/invalid response body (e.g. HTML error page).
var ErrNoContent = errors.New("server returned no content (HTML page instead of archive?)")

// Options control a download.
type Options struct {
	Session   string         // value for the BREEZESESSION cookie (optional)
	Cookies   []*http.Cookie // extra cookies, e.g. from a browser export
	Referer   string
	UserAgent string
	Attempts  int           // total attempts (default 3)
	Timeout   time.Duration // per-attempt body timeout (default 0 = no limit, ctx governs)
	// Progress is called with (done, total) as bytes arrive; total is -1 if unknown.
	Progress func(done, total int64)
	// Validate is called on the first bytes of the body to reject wrong formats.
	// If nil, no magic validation runs.
	Validate func(head []byte) error
}

func (o *Options) withDefaults() {
	if o.Attempts <= 0 {
		o.Attempts = 3
	}
	if o.UserAgent == "" {
		o.UserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"
	}
}

// File downloads url to dest (via dest+".part"), validating and renaming on success.
func File(ctx context.Context, client *http.Client, url, dest string, opt Options) error {
	opt.withDefaults()
	var lastErr error
	for attempt := 1; attempt <= opt.Attempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * time.Second): // 1s, 4s, 9s…
			}
		}
		err := attemptOnce(ctx, client, url, dest, opt)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrAuth) || errors.Is(err, ErrNoContent) || errors.Is(ctx.Err(), context.Canceled) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("download failed after %d attempts: %w", opt.Attempts, lastErr)
}

func attemptOnce(ctx context.Context, client *http.Client, url, dest string, opt Options) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", opt.UserAgent)
	if opt.Referer != "" {
		req.Header.Set("Referer", opt.Referer)
	}
	if opt.Session != "" {
		req.AddCookie(&http.Cookie{Name: "BREEZESESSION", Value: opt.Session})
	}
	for _, c := range opt.Cookies {
		req.AddCookie(c)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrAuth
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("server error: HTTP %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	part := dest + ".part"
	out, err := os.Create(part)
	if err != nil {
		return err
	}
	defer func() {
		out.Close()
		if err != nil {
			os.Remove(part)
		}
	}()

	body := io.Reader(resp.Body)
	if opt.Timeout > 0 {
		tctx, cancel := context.WithTimeout(ctx, opt.Timeout)
		defer cancel()
		body = ctxReader{tctx, resp.Body}
	} else {
		body = ctxReader{ctx, resp.Body}
	}

	total := resp.ContentLength
	if opt.Validate != nil {
		body = &validatingReader{r: body, fn: opt.Validate}
	}
	pr := &progressReader{r: body, total: total, fn: opt.Progress}
	if _, err := io.Copy(out, pr); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if opt.Progress != nil && total > 0 {
		opt.Progress(total, total)
	}
	return os.Rename(part, dest)
}

// validatingReader peeks the first bytes, runs validate, then passes everything through.
type validatingReader struct {
	r    io.Reader
	fn   func(head []byte) error
	head []byte
	done bool
}

func (v *validatingReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	if n > 0 && !v.done {
		v.head = append(v.head, p[:n]...)
		if len(v.head) >= 4 || err != nil {
			v.done = true
			if e := v.fn(v.head); e != nil {
				return 0, e
			}
		}
	}
	return n, err
}

// progressReader throttles Progress callbacks to ~10/s.
type progressReader struct {
	r        io.Reader
	total    int64
	done     int64
	fn       func(done, total int64)
	lastCall time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.done += int64(n)
		if p.fn != nil && time.Since(p.lastCall) > 100*time.Millisecond {
			p.lastCall = time.Now()
			p.fn(p.done, p.total)
		}
	}
	return n, err
}

// ctxReader aborts reads when ctx is done (body reads don't honor ctx by default).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// IsHTML reports whether head looks like an HTML page (login/error) instead of expected binary.
func IsHTML(head []byte) bool {
	s := strings.ToLower(strings.TrimSpace(string(head)))
	return strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html") || strings.HasPrefix(s, "<?xml")
}

// ValidateZIP rejects non-ZIP bodies (including HTML login pages).
func ValidateZIP(head []byte) error {
	if len(head) >= 2 && head[0] == 'P' && head[1] == 'K' {
		return nil
	}
	if IsHTML(head) {
		return fmt.Errorf("%w: got HTML (login page?)", ErrNoContent)
	}
	return fmt.Errorf("%w: missing ZIP signature", ErrNoContent)
}
