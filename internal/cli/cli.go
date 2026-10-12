// Package cli implements the ac-rec command line.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/engine"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/recordingurl"
)

const usage = `ac-rec — Adobe Connect offline recording downloader/reconstructor

Usage:
  ac-rec download <recording-url> [flags]     download archive + reconstruct
  ac-rec reconstruct <archive.zip> [flags]    reconstruct from a local ZIP

Flags:
  --session <tok>     BREEZESESSION cookie value (logged-in session)
  --cookie <k=v>      extra cookie (repeatable), e.g. --cookie BreezeLoginCookie=…
  --out <dir>         output parent directory (default ./recordings)
  --name <title>      output folder name (default: recording page title / zip name)
  --overwrite         replace existing output
  --mp3               also export recording.mp3 (audio only)
  --raw               full-frame video at source resolution (skip 720p layout)

Default output is a 1280x720 video approximating the presented layout:
content on the left, attendee list + chat replay burned into a right sidebar.

Get a session token: log in via browser → DevTools → Network → any request →
Request Headers → Cookie: BREEZESESSION=…
`

// Main runs the CLI and returns a process exit code.
func Main(args []string) int {
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, rest := args[1], args[2:]

	switch cmd {
	case "download", "reconstruct":
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		session   = fs.String("session", "", "BREEZESESSION cookie value")
		outDir    = fs.String("out", "", "output parent directory")
		name      = fs.String("name", "", "output folder name")
		overwrite = fs.Bool("overwrite", false, "replace existing output")
		raw       = fs.Bool("raw", false, "full-frame video at source resolution (skip 720p layout)")
		mp3       = fs.Bool("mp3", false, "also export recording.mp3")
		cookies   = cookieList{}
	)
	fs.Var(&cookies, "cookie", "extra cookie as NAME=VALUE (repeatable)")
	// allow flags before AND after the positional argument
	var positional []string
	remaining := rest
	for {
		if err := fs.Parse(remaining); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		remaining = fs.Args()[1:]
	}
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "expected exactly one argument (URL or ZIP path)")
		return 2
	}
	target := positional[0]

	if !engine.HasFFmpeg() {
		fmt.Fprintln(os.Stderr, "error: ffmpeg and ffprobe must be installed and on PATH")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opt := engine.Options{
		Session:   *session,
		Cookies:   cookies.cookies(),
		Title:     *name,
		OutDir:    *outDir,
		Overwrite: *overwrite,
		Raw:       *raw,
		MP3:       *mp3,
		Status:    func(s string) { fmt.Fprintf(os.Stderr, "\r\033[K%s\n", s) },
		Progress:  newProgress(os.Stderr),
	}

	if cmd == "reconstruct" {
		opt.ZipPath = target
	} else {
		ru, err := recordingurl.Parse(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 2
		}
		opt.ZipURL = ru.ZipURL()
		if opt.Title == "" {
			opt.Title = fetchTitle(ctx, ru.PageURL(), *session, cookies.cookies())
		}
		if opt.Title == "" { // page unreachable/unauthorized → recording id as name
			opt.Title = ru.ID
		}
	}

	rep, err := engine.Run(ctx, opt)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "\ncancelled")
			return 130
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	fmt.Printf("\nDone\n")
	kind := "video"
	if rep.Layout {
		kind = "video (720p layout)"
	}
	fmt.Printf("  %s:     %s (%s, %s)\n", kind, rep.VideoPath, fmtDur(rep.DurationMs), rep.Mode)
	if rep.MP3Path != "" {
		fmt.Printf("  mp3:       %s\n", rep.MP3Path)
	}
	if rep.ChatMessages > 0 {
		fmt.Printf("  chat:      %d messages\n", rep.ChatMessages)
	}
	if rep.AttendeeCount > 0 {
		fmt.Printf("  attendees: %d\n", rep.AttendeeCount)
	}
	for _, w := range rep.Warnings {
		fmt.Printf("  warning:   %s\n", w)
	}
	return 0
}

// ---- helpers -------------------------------------------------------------

type cookieList []string

func (c *cookieList) String() string { return strings.Join(*c, ",") }
func (c *cookieList) Set(v string) error {
	if !strings.Contains(v, "=") {
		return fmt.Errorf("cookie must be NAME=VALUE, got %q", v)
	}
	*c = append(*c, v)
	return nil
}

func (c cookieList) cookies() []*http.Cookie {
	var out []*http.Cookie
	for _, kv := range c {
		k, v, _ := strings.Cut(kv, "=")
		out = append(out, &http.Cookie{Name: k, Value: strings.TrimSpace(v)})
	}
	return out
}

var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// fetchTitle grabs the recording page title (best-effort; falls back to id).
func fetchTitle(ctx context.Context, pageURL, session string, cookies []*http.Cookie) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/120.0 Safari/537.36")
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "BREEZESESSION", Value: session})
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	m := titleRe.FindSubmatch(body)
	if m == nil {
		return ""
	}
	title := strings.TrimSpace(string(m[1]))
	for _, suf := range []string{" | Adobe Connect", " - Adobe Connect", " · Adobe Connect"} {
		title = strings.TrimSuffix(title, suf)
	}
	title = strings.TrimSpace(title)
	if title == "" || strings.EqualFold(title, "Adobe Connect") {
		return ""
	}
	return title
}

func fmtDur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// newProgress prints throttled single-line progress on w.
func newProgress(w io.Writer) func(stage string, done, total int64) {
	var last time.Time
	var lastStage string
	return func(stage string, done, total int64) {
		if time.Since(last) < 200*time.Millisecond && stage == lastStage {
			return
		}
		last = time.Now()
		lastStage = stage
		if stage == "download" { // bytes
			if total > 0 {
				fmt.Fprintf(w, "\r\033[Kdownload %5.1f%% (%.1f / %.1f MB)",
					float64(done)/float64(total)*100,
					float64(done)/(1<<20), float64(total)/(1<<20))
			} else {
				fmt.Fprintf(w, "\r\033[Kdownload %.1f MB", float64(done)/(1<<20))
			}
			return
		}
		if total > 0 { // milliseconds
			fmt.Fprintf(w, "\r\033[K%s %5.1f%% (%s / %s)", stage,
				float64(done)/float64(total)*100, fmtDur(done), fmtDur(total))
		} else {
			fmt.Fprintf(w, "\r\033[K%s %s", stage, fmtDur(done))
		}
	}
}
