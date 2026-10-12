// Package reconstruct turns extracted recording FLVs + timeline into one MP4
// using a single-pass FFmpeg filter graph (gaps filled with black video /
// silence, VP6 & Nellymoser re-encoded to H.264/AAC).
package reconstruct

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/timeline"
)

// Options for a reconstruction run.
type Options struct {
	Dir        string // extracted archive directory (contains mainstream.xml + FLVs)
	Out        string // output .mp4 path
	Preset     string // x264 preset (default "veryfast")
	CRF        int    // x24 quality (default 23)
	FPS        int    // output frame rate (default 25)
	Overwrite  bool
	Progress   func(doneMs, totalMs int64)
	Status     func(msg string) // human-readable phase updates
	FFmpegPath string           // default "ffmpeg"
	FFprobePath string          // default "ffprobe"
}

// Result describes what was produced.
type Result struct {
	VideoSegments int
	AudioSegments int
	Mode          string // "av", "video-only", "audio-only"
}

const (
	defaultPreset   = "veryfast"
	defaultCRF      = 23
	defaultFPS      = 25
	audioRate       = 48000
	audioChannels   = "stereo"
	fallbackWidth   = 1280
	fallbackHeight  = 720
	segmentOverhead = 2 * time.Second // safety per-segment read slack
)

// Run reconstructs the recording. It is cancellable via ctx.
func Run(ctx context.Context, opt Options) (Result, error) {
	if opt.Preset == "" {
		opt.Preset = defaultPreset
	}
	if opt.CRF == 0 {
		opt.CRF = defaultCRF
	}
	if opt.FPS == 0 {
		opt.FPS = defaultFPS
	}
	if opt.FFmpegPath == "" {
		opt.FFmpegPath = "ffmpeg"
	}
	if opt.FFprobePath == "" {
		opt.FFprobePath = "ffprobe"
	}

	tl, err := timeline.ParseFile(filepath.Join(opt.Dir, "mainstream.xml"))
	if err != nil {
		return Result{}, err
	}
	vidSegs := tl.Video()
	audSegs := tl.Audio()
	totalMs := tl.DurationMs
	for _, s := range tl.Segments {
		if s.EndMs > totalMs {
			totalMs = s.EndMs
		}
	}
	if totalMs <= 0 {
		return Result{}, errors.New("timeline has zero duration")
	}

	mode := "av"
	switch {
	case len(vidSegs) == 0 && len(audSegs) == 0:
		return Result{}, errors.New("timeline has no video or audio segments")
	case len(vidSegs) == 0:
		mode = "audio-only"
	case len(audSegs) == 0:
		mode = "video-only" // we add a silent track
	}

	for _, s := range append(append([]timeline.Segment{}, vidSegs...), audSegs...) {
		p := filepath.Join(opt.Dir, s.FileName())
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			return Result{}, fmt.Errorf("missing segment file: %s", s.FileName())
		}
	}

	width, height := fallbackWidth, fallbackHeight
	if mode != "audio-only" {
		if w, h, err := probeSize(ctx, opt.FFprobePath, filepath.Join(opt.Dir, vidSegs[0].FileName())); err == nil {
			width, height = w, h
		}
	}
	if width%2 != 0 {
		width++
	}
	if height%2 != 0 {
		height++
	}

	spec := buildSpec(opt.Dir, vidSegs, audSegs, totalMs, width, height, opt.FPS, mode)
	args, err := spec.args(opt)
	if err != nil {
		return Result{}, err
	}

	if opt.Status != nil {
		opt.Status(fmt.Sprintf("reconstructing %s (%d video / %d audio segments, %s)",
			mode, len(vidSegs), len(audSegs), (time.Duration(totalMs) * time.Millisecond).Truncate(time.Second)))
	}
	if err := runFFmpeg(ctx, opt, args, totalMs); err != nil {
		return Result{}, err
	}
	return Result{VideoSegments: len(vidSegs), AudioSegments: len(audSegs), Mode: mode}, nil
}

// ---- graph spec ----------------------------------------------------------

type node struct {
	label string // output label, e.g. "[v0]"
	src   []string
	filter string
}

type spec struct {
	nodes   []node
	video   []string // ordered labels feeding video concat (empty if audio-only)
	audio   []string
}

func buildSpec(dir string, vid, aud []timeline.Segment, totalMs int64, w, h, fps int, mode string) *spec {
	sp := &spec{}
	idx := 0

	if mode != "audio-only" {
		endMs := int64(0)
		vf := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,fps=%d,format=yuv420p", w, h, w, h, fps)
		addGap := func(durMs int64) {
			if durMs < 50 {
				return
			}
			sec := float64(durMs) / 1000
			l := fmt.Sprintf("[v%d]", idx)
			sp.video = append(sp.video, l)
			sp.nodes = append(sp.nodes, node{
				label: l,
				src:   []string{"-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%dx%d:r=%d:d=%.3f", w, h, fps, sec)},
				filter: fmt.Sprintf("[%d:v]format=yuv420p%s", idx, l),
			})
			idx++
		}
		for _, s := range vid {
			addGap(s.StartMs - endMs)
			sec := float64(s.DurationMs()) / 1000
			l := fmt.Sprintf("[v%d]", idx)
			sp.video = append(sp.video, l)
			sp.nodes = append(sp.nodes, node{
				label:  l,
				src:    []string{"-t", fmt.Sprintf("%.3f", sec), "-i", filepath.Join(dir, s.FileName())},
				filter: fmt.Sprintf("[%d:v]%s%s", idx, vf, l),
			})
			idx++
			if s.EndMs > endMs {
				endMs = s.EndMs
			}
		}
		addGap(totalMs - endMs)
	}

	if mode != "video-only" {
		endMs := int64(0)
		afmt := fmt.Sprintf("aresample=%d,aformat=sample_fmts=fltp:sample_rates=%d:channel_layouts=%s", audioRate, audioRate, audioChannels)
		addSilence := func(durMs int64) {
			if durMs < 50 {
				return
			}
			sec := float64(durMs) / 1000
			l := fmt.Sprintf("[a%d]", idx)
			sp.audio = append(sp.audio, l)
			sp.nodes = append(sp.nodes, node{
				label: l,
				src:   []string{"-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=%d:cl=%s:d=%.3f", audioRate, audioChannels, sec)},
				filter: fmt.Sprintf("[%d:a]%s%s", idx, afmt, l),
			})
			idx++
		}
		for _, s := range aud {
			addSilence(s.StartMs - endMs)
			sec := float64(s.DurationMs()) / 1000
			l := fmt.Sprintf("[a%d]", idx)
			sp.audio = append(sp.audio, l)
			sp.nodes = append(sp.nodes, node{
				label:  l,
				src:    []string{"-t", fmt.Sprintf("%.3f", sec), "-i", filepath.Join(dir, s.FileName())},
				filter: fmt.Sprintf("[%d:a]%s%s", idx, afmt, l),
			})
			idx++
			if s.EndMs > endMs {
				endMs = s.EndMs
			}
		}
		if len(aud) == 0 {
			addSilence(totalMs) // silent track so players see real duration
		} else {
			addSilence(totalMs - endMs)
		}
	}
	return sp
}

func (s *spec) args(opt Options) ([]string, error) {
	if len(s.video) == 0 && len(s.audio) == 0 {
		return nil, errors.New("empty filter graph")
	}

	var fc strings.Builder
	for _, n := range s.nodes {
		fc.WriteString(n.filter)
		fc.WriteByte(';')
	}
	videoOut, audioOut := "", ""
	if len(s.video) > 0 {
		if len(s.video) == 1 {
			videoOut = s.video[0] // per-input filter chain already labels it
		} else {
			fc.WriteString(strings.Join(s.video, "") + fmt.Sprintf("concat=n=%d:v=1:a=0[vo];", len(s.video)))
			videoOut = "[vo]"
		}
	}
	if len(s.audio) > 0 {
		if len(s.audio) == 1 {
			audioOut = s.audio[0]
		} else {
			fc.WriteString(strings.Join(s.audio, "") + fmt.Sprintf("concat=n=%d:v=0:a=1[ao];", len(s.audio)))
			audioOut = "[ao]"
		}
	}

	ffmpeg := opt.FFmpegPath
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	args := []string{
		ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-progress", "pipe:1", "-nostats",
	}
	if opt.Overwrite {
		args = append(args, "-y")
	} else {
		args = append(args, "-n")
	}
	for _, n := range s.nodes {
		args = append(args, n.src...)
	}
	if fc.Len() > 0 {
		args = append(args, "-filter_complex", strings.TrimSuffix(fc.String(), ";"))
	}
	if videoOut != "" {
		args = append(args, "-map", videoOut,
			"-c:v", "libx264", "-preset", opt.Preset,
			"-crf", strconv.Itoa(opt.CRF), "-pix_fmt", "yuv420p")
	}
	if audioOut != "" {
		args = append(args, "-map", audioOut, "-c:a", "aac", "-b:a", "128k")
	}
	if videoOut != "" {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, opt.Out)
	return args, nil
}

// ---- ffmpeg execution ----------------------------------------------------

func runFFmpeg(ctx context.Context, opt Options, args []string, totalMs int64) error {
	if err := os.MkdirAll(filepath.Dir(opt.Out), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", args[0], err)
	}

	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1024), 1024)
		for sc.Scan() {
			line := sc.Text()
			if opt.Progress == nil || totalMs <= 0 {
				continue
			}
			if v, ok := progressOutTime(line); ok {
				doneMs := int64(v * 1000) // v is media seconds processed → ms
				if doneMs > totalMs {
					doneMs = totalMs
				}
				opt.Progress(doneMs, totalMs)
			}
		}
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return fmt.Errorf("ffmpeg: %v\n%s", err, stderr.String())
		}
	}
	st, err := os.Stat(opt.Out)
	if err != nil {
		return fmt.Errorf("output missing: %w", err)
	}
	if st.Size() < 1024 {
		return errors.New("ffmpeg produced an empty output")
	}
	if opt.Progress != nil && totalMs > 0 {
		opt.Progress(totalMs, totalMs)
	}
	return nil
}

// progressOutTime parses "out_time_ms=1234567" (ffmpeg reports microseconds
// under this key) → seconds. Also handles out_time=HH:MM:SS.uuuuuu.
func progressOutTime(line string) (float64, bool) {
	k, v, found := strings.Cut(line, "=")
	if !found {
		return 0, false
	}
	switch k {
	case "out_time_us", "out_time_ms":
		us, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		return us / 1e6, true
	case "out_time":
		parts := strings.Split(strings.TrimSpace(v), ":")
		if len(parts) != 3 {
			return 0, false
		}
		var sec float64
		for _, p := range parts {
			f, err := strconv.ParseFloat(p, 64)
			if err != nil {
				return 0, false
			}
			sec = sec*60 + f
		}
		return sec, true
	}
	return 0, false
}

func probeSize(ctx context.Context, ffprobe, file string) (int, int, error) {
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=p=0:s=x", file)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ffprobe %s: %w", filepath.Base(file), err)
	}
	var w, h int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("ffprobe %s: bad size %q", filepath.Base(file), strings.TrimSpace(string(out)))
	}
	return w, h, nil
}

// ProbeDuration returns media duration in milliseconds.
func ProbeDuration(ctx context.Context, ffprobe, file string) (int64, error) {
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", file)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	sec, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("bad duration %q", strings.TrimSpace(string(out)))
	}
	return int64(sec * 1000), nil
}

// tailBuffer keeps the last 4 KiB of stderr for error reporting.
type tailBuffer struct {
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return strings.TrimSpace(string(t.buf)) }
