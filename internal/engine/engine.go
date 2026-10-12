// Package engine wires the pipeline together: archive in, video + sidecars out.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/assets"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/download"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/reconstruct"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/timeline"
)

// Options for a full pipeline run.
type Options struct {
	ZipPath   string // existing archive (reconstruct-only mode)
	ZipURL    string // download mode (ignored if ZipPath set)
	Session   string
	Cookies   []*http.Cookie // extra browser cookies
	Title     string         // output folder name (default: archive stem)
	OutDir    string         // parent output directory (default: cwd)
	Overwrite bool
	Raw       bool   // full-frame output instead of 720p layout composition
	MP3       bool   // also export recording.mp3
	Width     int    // output resolution width (0 = 1280x720 default)
	Height    int    // output resolution height (0 = 1280x720 default)
	Username  string // .env credentials: logged in via /api/xml before download
	Password  string
	Status    func(string)
	Progress  func(stage string, done, total int64)
}

// Report summarizes a completed run.
type Report struct {
	OutDir        string   `json:"-"`
	VideoPath     string   `json:"video"`
	MP3Path       string   `json:"mp3,omitempty"`
	DurationMs    int64    `json:"duration_ms"`
	ExpectedMs    int64    `json:"expected_duration_ms"`
	Mode          string   `json:"mode"`
	Layout        bool     `json:"layout"`
	VideoSegments int      `json:"video_segments"`
	AudioSegments int      `json:"audio_segments"`
	ChatMessages  int      `json:"chat_messages"`
	AttendeeCount int      `json:"attendee_count"`
	Warnings      []string `json:"warnings,omitempty"`
	ArchiveBytes  int64    `json:"archive_bytes"`
	VideoBytes    int64    `json:"video_bytes"`
	MP3Bytes      int64    `json:"mp3_bytes,omitempty"`
}

// ErrUnsupported marks recordings this tool cannot process.
var ErrUnsupported = errors.New("unsupported recording")

const toleranceMs = 10_000 // duration validation window (10s)

// Run executes the pipeline for one recording.
func Run(ctx context.Context, opt Options) (Report, error) {
	var rep Report
	status := opt.Status
	if status == nil {
		status = func(string) {}
	}

	work, err := os.MkdirTemp("", "acrec-*")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(work)

	zipPath := opt.ZipPath
	if zipPath == "" {
		cookies := opt.Cookies
		if opt.Username != "" {
			status("logging in")
			cs, err := download.Login(ctx, &http.Client{}, originOf(opt.ZipURL), opt.Username, opt.Password)
			if err != nil {
				return rep, err
			}
			cookies = append(cookies, cs...)
		}
		status("downloading archive")
		zipPath = filepath.Join(work, "recording.zip")
		if err := download.File(ctx, &http.Client{}, opt.ZipURL, zipPath,
			download.Options{
				Session:  opt.Session,
				Cookies:  cookies,
				Referer:  refererOf(opt.ZipURL),
				Validate: download.ValidateZIP,
				Progress: func(d, t int64) {
					if opt.Progress != nil {
						opt.Progress("download", d, t)
					}
				},
			}); err != nil {
			return rep, err
		}
	}
	if st, err := os.Stat(zipPath); err == nil {
		rep.ArchiveBytes = st.Size()
	}

	title := opt.Title
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(zipPath), filepath.Ext(zipPath))
	}
	outDir := filepath.Join(defaultOutDir(opt.OutDir), sanitize(title))
	if opt.Overwrite {
		_ = os.RemoveAll(outDir)
	}
	if _, err := os.Stat(filepath.Join(outDir, "recording.mp4")); err == nil && !opt.Overwrite {
		return rep, fmt.Errorf("output already exists: %s (use --overwrite)", outDir)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return rep, err
	}
	rep.OutDir = outDir

	status("extracting archive")
	rawDir := filepath.Join(work, "raw")
	if err := assets.Extract(zipPath, rawDir, func(d, t int64) {
		if opt.Progress != nil {
			opt.Progress("extract", d, t)
		}
	}); err != nil {
		return rep, err
	}

	mainstream := assets.Find(rawDir, "mainstream.xml")
	if mainstream == "" {
		return rep, fmt.Errorf("%w: archive has no mainstream.xml (not a Standard AV recording, or browser-capture fallback needed)", ErrUnsupported)
	}
	tl, err := timeline.ParseFile(mainstream)
	if err != nil {
		return rep, err
	}
	rep.ExpectedMs = tl.DurationMs

	// Sidecar data is also layout input (chat replay + attendee panel).
	chat := parseChat(rawDir)
	rep.ChatMessages = len(chat)
	var attendees []string
	if f := assets.Find(rawDir, "indexstream.xml"); f != "" {
		if names, err := assets.ParseAttendees(f); err == nil {
			attendees = names
			rep.AttendeeCount = len(names)
		}
	}

	// Reconstruct media.
	outVideo := filepath.Join(outDir, "recording.mp4")
	rep.VideoPath = outVideo
	res, err := reconstruct.Run(ctx, reconstruct.Options{
		Dir:       rawDir,
		Out:       outVideo,
		Overwrite: true,
		Raw:       opt.Raw,
		Width:     opt.Width,
		Height:    opt.Height,
		Chat:      chat,
		Attendees: attendees,
		Status:    status,
		Progress: func(d, t int64) {
			if opt.Progress != nil {
				opt.Progress("reconstruct", d, t)
			}
		},
	})
	if err != nil {
		return rep, err
	}
	rep.Mode = res.Mode
	rep.Layout = res.Layout
	rep.VideoSegments = res.VideoSegments
	rep.AudioSegments = res.AudioSegments

	// Validate duration.
	if d, err := reconstruct.ProbeDuration(ctx, "ffprobe", outVideo); err == nil {
		rep.DurationMs = d
		diff := d - rep.ExpectedMs
		if diff < 0 {
			diff = -diff
		}
		if diff > toleranceMs {
			rep.Warnings = append(rep.Warnings,
				fmt.Sprintf("output duration %s differs from timeline %s by %s",
					ms(d), ms(rep.ExpectedMs), ms(diff)))
		}
	} else {
		rep.Warnings = append(rep.Warnings, "could not probe output duration: "+err.Error())
	}
	if st, err := os.Stat(outVideo); err == nil {
		rep.VideoBytes = st.Size()
	}

	// Optional MP3 audio-only export.
	if opt.MP3 {
		status("exporting mp3")
		mp3Path := filepath.Join(outDir, "recording.mp3")
		if err := reconstruct.ExtractMP3(ctx, "ffmpeg", outVideo, mp3Path); err != nil {
			rep.Warnings = append(rep.Warnings, err.Error())
		} else {
			rep.MP3Path = mp3Path
			if st, err := os.Stat(mp3Path); err == nil {
				rep.MP3Bytes = st.Size()
			}
		}
	}

	// Sidecars: chat log.
	if len(chat) > 0 {
		var b strings.Builder
		for _, m := range chat {
			b.WriteString(m.Format())
			b.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(outDir, "chat_log.txt"), []byte(b.String()), 0o644); err != nil {
			rep.Warnings = append(rep.Warnings, "chat write failed: "+err.Error())
		}
	} else {
		rep.Warnings = append(rep.Warnings, "no chat messages found")
	}

	// Sidecars: attendees.
	if len(attendees) > 0 {
		_ = os.WriteFile(filepath.Join(outDir, "attendees.txt"),
			[]byte(strings.Join(attendees, "\n")+"\n"), 0o644)
	}

	if err := writeJSON(filepath.Join(outDir, "metadata.json"), rep); err != nil {
		rep.Warnings = append(rep.Warnings, "metadata write failed: "+err.Error())
	}
	status(fmt.Sprintf("done: %s (%s)", outVideo, ms(rep.DurationMs)))
	return rep, nil
}

func parseChat(rawDir string) []assets.ChatMessage {
	// Glob ftchat*.xml explicitly: FindPrefix would match the binary ftchat0.flv
	// first (lexically before .xml) and the XML parser would fail.
	var out []assets.ChatMessage
	if matches, err := filepath.Glob(filepath.Join(rawDir, "ftchat*.xml")); err == nil {
		for _, p := range matches {
			fh, err := os.Open(p)
			if err != nil {
				continue
			}
			chat, err := assets.ParseChat(fh)
			fh.Close()
			if err == nil {
				out = append(out, chat...)
			}
		}
	}
	if len(out) > 0 {
		sort.Slice(out, func(i, j int) bool { return out[i].TimeMs < out[j].TimeMs })
		return out
	}
	if f := assets.Find(rawDir, "sco_metadata.xml"); f != "" {
		if chat, err := func() ([]assets.ChatMessage, error) {
			fh, err := os.Open(f)
			if err != nil {
				return nil, err
			}
			defer fh.Close()
			return assets.ParseChatSections(fh)
		}(); err == nil {
			return chat
		}
	}
	return nil
}

func defaultOutDir(d string) string {
	if d != "" {
		return d
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return filepath.Join(wd, "recordings")
}

// sanitize makes a safe folder name; keeps unicode letters/digits.
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "recording"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|' || r < 0x20:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), ". ")
	if out == "" {
		return "recording"
	}
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

func refererOf(zipURL string) string {
	// https://host/id/output/x.zip?… → https://host/id/
	if i := strings.Index(zipURL, "/output/"); i > 0 {
		return zipURL[:i] + "/"
	}
	return zipURL
}

func originOf(zipURL string) string {
	// https://host/id/output/x.zip?… → https://host
	if i := strings.Index(zipURL, "://"); i > 0 {
		rest := zipURL[i+3:]
		if j := strings.Index(rest, "/"); j > 0 {
			return zipURL[:i+3+j]
		}
	}
	return zipURL
}

func ms(v int64) string {
	return (time.Duration(v) * time.Millisecond).Truncate(time.Second).String()
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// HasFFmpeg reports whether ffmpeg and ffprobe are on PATH.
func HasFFmpeg() bool {
	for _, b := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(b); err != nil {
			return false
		}
	}
	return true
}
