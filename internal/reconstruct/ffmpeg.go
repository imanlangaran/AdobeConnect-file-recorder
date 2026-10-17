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

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/assets"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/timeline"
)

// Options for a reconstruction run.
type Options struct {
	Dir         string // extracted archive directory (contains mainstream.xml + FLVs)
	Out         string // output .mp4 path
	Preset      string // x264 preset (default "veryfast")
	CRF         int    // x24 quality (default 23)
	FPS         int    // output frame rate (default 25)
	Overwrite   bool
	Raw         bool // skip layout composition (full-frame, source resolution)
	Width       int  // output resolution width (0 = 1280x720 default)
	Height      int  // output resolution height (0 = 1280x720 default)
	Chat        []assets.ChatMessage
	Attendees   []string
	Progress    func(doneMs, totalMs int64)
	Status      func(msg string) // human-readable phase updates
	FFmpegPath  string           // default "ffmpeg"
	FFprobePath string           // default "ffprobe"
}

// Result describes what was produced.
type Result struct {
	VideoSegments int
	AudioSegments int
	Mode          string // "av", "video-only", "audio-only"
	Layout        bool   // layout composition applied
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
	// chat history bounding box on the sidebar (drawn on the canvas — NOT an
	// ASS event: libass collision-avoidance shifts text events overlapping a
	// \p1 drawing, which pushed the chat out of the box). Y/H come from the
	// panel geometry (ass.go): the box spans the recorded video's rect, so
	// its height always matches the video.
	chatBoxX = 966
	chatBoxW = 310
)

// Run reconstructs the recording. It is cancellable via ctx.
func Run(ctx context.Context, opt Options) (Result, error) {
	if opt.Preset == "" {
		opt.Preset = defaultPreset
	}
	if opt.CRF <= 0 { // 0 = unset (CRF 0 is pointless in practice); negatives invalid
		opt.CRF = defaultCRF
	}
	if opt.FPS <= 0 {
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

	for _, s := range append(append([]timeline.Segment{}, vidSegs...), audSegs...) {
		p := filepath.Join(opt.Dir, s.FileName())
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			return Result{}, fmt.Errorf("missing segment file: %s", s.FileName())
		}
	}

	// The timeline can claim a stream the file does not carry (a cameraVoip
	// publisher that only ever sent video, a truncated download): ffmpeg then
	// kills the whole run with "Stream specifier ':a' … matches no streams".
	// Probe each segment and drop the unusable ones instead.
	var skipped []string
	srcW, srcH := 0, 0 // probed source size (0 = unknown → full-height panel)
	usable := func(segs []timeline.Segment, wantVideo bool) []timeline.Segment {
		var out []timeline.Segment
		for _, s := range segs {
			hasV, hasA, w, h, err := probeStreams(ctx, opt.FFprobePath, filepath.Join(opt.Dir, s.FileName()))
			switch {
			case err != nil:
				skipped = append(skipped, err.Error())
			case wantVideo && !hasV, !wantVideo && !hasA:
				skipped = append(skipped, s.FileName())
			default:
				if wantVideo && srcW == 0 {
					srcW, srcH = w, h
				}
				out = append(out, s)
			}
		}
		return out
	}
	vidSegs, audSegs = usable(vidSegs, true), usable(audSegs, false)
	if len(skipped) > 0 && opt.Status != nil {
		opt.Status(fmt.Sprintf("skipping %d unusable segment file(s): %s",
			len(skipped), strings.Join(skipped, ", ")))
	}
	if len(vidSegs) == 0 && len(audSegs) == 0 {
		return Result{}, fmt.Errorf("timeline has no usable video or audio segments (skipped: %s)",
			strings.Join(skipped, ", "))
	}

	mode := "av"
	switch {
	case len(vidSegs) == 0:
		mode = "audio-only"
	case len(audSegs) == 0:
		mode = "video-only" // we add a silent track
	}

	width, height := fallbackWidth, fallbackHeight
	if mode != "audio-only" && srcW > 0 {
		width, height = srcW, srcH
	}
	if width%2 != 0 {
		width++
	}
	if height%2 != 0 {
		height++
	}
	if opt.Width > 0 && opt.Height > 0 {
		width, height = opt.Width, opt.Height // explicit --resolution target
	}

	// Layout composition: 1280x720 canvas, content box left, sidebar with
	// burned-in attendee list + chat replay on the right. The sidebar box
	// spans the recorded video's letterboxed rect — same height as the
	// video itself.
	layout := !opt.Raw && mode != "audio-only"
	assPath, p := "", panel{}
	if layout {
		vw, vh := contentBox()
		width, height = vw, vh
		vTop, vH := 0, vh
		if srcW > 0 && srcH > 0 {
			vTop, vH = fitRect(srcW, srcH, vw, vh)
		}
		p = newPanel(vTop, vH, len(opt.Attendees))
		if len(opt.Chat) > 0 || len(opt.Attendees) > 0 {
			assPath = filepath.Join(opt.Dir, "layout.ass")
			if err := os.WriteFile(assPath, []byte(BuildASS(opt.Chat, opt.Attendees, totalMs, p)), 0o644); err != nil {
				return Result{}, fmt.Errorf("write subtitles: %w", err)
			}
		}
	}

	spec := buildSpec(opt.Dir, vidSegs, audSegs, totalMs, width, height, opt.FPS, mode, layout)
	spec.layout = layout
	spec.assPath = assPath
	spec.chatBoxY, spec.chatBoxH = p.boxY, p.boxH
	if layout && opt.Width > 0 && (opt.Width != canvasW || opt.Height != canvasH) {
		spec.outW, spec.outH = opt.Width, opt.Height // downscale composed canvas
	}
	if layout {
		sec := float64(totalMs) / 1000
		spec.canvasSrc = []string{"-f", "lavfi", "-i",
			fmt.Sprintf("color=c=black:s=%dx%d:r=%d:d=%.3f", canvasW, canvasH, opt.FPS, sec)}
	}
	args, err := spec.args(opt)
	if err != nil {
		return Result{}, err
	}

	if opt.Status != nil {
		what := "full-frame"
		if layout {
			what = "layout"
		}
		opt.Status(fmt.Sprintf("reconstructing %s %s (%d video / %d audio segments, %s)",
			mode, what, len(vidSegs), len(audSegs), (time.Duration(totalMs) * time.Millisecond).Truncate(time.Second)))
	}
	if err := runFFmpeg(ctx, opt, args, totalMs); err != nil {
		return Result{}, err
	}
	return Result{VideoSegments: len(vidSegs), AudioSegments: len(audSegs), Mode: mode, Layout: layout}, nil
}

// ---- graph spec ----------------------------------------------------------

type node struct {
	label  string // output label, e.g. "[v0]"
	src    []string
	filter string
}

type spec struct {
	nodes       []node
	video       []string // ordered labels feeding video concat (empty if audio-only)
	audio       []string // placed audio labels (mixed, timeline-aligned)
	totalMs     int64    // timeline duration: pad/trim audio to exactly this
	layout      bool     // canvas + sidebar composition
	canvasSrc   []string // lavfi canvas input (placed as input 0)
	assPath     string   // burned-in subtitles file ("" = none)
	outW, outH  int      // output resolution override for layout mode (0 = canvas)
	chatBoxY    int      // chat history box top (panel geometry)
	chatBoxH    int      // chat history box height
	inputOffset int      // input index of the first node (1 when canvas present)
}

func buildSpec(dir string, vid, aud []timeline.Segment, totalMs int64, w, h, fps int, mode string, layout bool) *spec {
	sp := &spec{}
	idx := 0
	if layout {
		sp.inputOffset = 1 // canvas occupies input 0
	}
	in := func() int { return idx + sp.inputOffset }

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
				label:  l,
				src:    []string{"-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%dx%d:r=%d:d=%.3f", w, h, fps, sec)},
				filter: fmt.Sprintf("[%d:v]format=yuv420p%s", in(), l),
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
				filter: fmt.Sprintf("[%d:v]%s%s", in(), vf, l),
			})
			idx++
			if s.EndMs > endMs {
				endMs = s.EndMs
			}
		}
		addGap(totalMs - endMs)
	}

	if mode != "video-only" {
		afmt := fmt.Sprintf("aresample=%d,aformat=sample_fmts=fltp:sample_rates=%d:channel_layouts=%s", audioRate, audioRate, audioChannels)
		if len(aud) == 0 {
			// silent track so players see real duration
			l := fmt.Sprintf("[a%d]", idx)
			sp.audio = append(sp.audio, l)
			sp.nodes = append(sp.nodes, node{
				label:  l,
				src:    []string{"-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=%d:cl=%s:d=%.3f", audioRate, audioChannels, float64(totalMs)/1000)},
				filter: fmt.Sprintf("[%d:a]%s%s", in(), afmt, l),
			})
			idx++
		}
		// Each segment is placed at its timeline position (adelay) and mixed,
		// not concatenated: segments can overlap (two publishers live at once)
		// and file content duration differs from the timeline slot — plain
		// concatenation drifts by seconds over an hour-long recording.
		for _, s := range aud {
			sec := float64(s.DurationMs()) / 1000
			l := fmt.Sprintf("[a%d]", idx)
			sp.audio = append(sp.audio, l)
			sp.nodes = append(sp.nodes, node{
				label: l,
				src:   []string{"-t", fmt.Sprintf("%.3f", sec), "-i", filepath.Join(dir, s.FileName())},
				filter: fmt.Sprintf("[%d:a]%s,asetpts=PTS-STARTPTS,atrim=0:%.3f,adelay=%d:all=1%s",
					in(), afmt, sec, s.StartMs, l),
			})
			idx++
		}
	}
	sp.totalMs = totalMs
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
		dur := float64(s.totalMs) / 1000
		tail := fmt.Sprintf("aresample=%d:async=1,apad=whole_dur=%.3f,atrim=0:%.3f[aout];",
			audioRate, dur, dur) // fill timestamp holes, pad/trim to exact timeline length
		if len(s.audio) == 1 {
			fc.WriteString(s.audio[0] + tail)
		} else {
			// segments are already placed at their timeline offsets → mix, not concat
			fc.WriteString(strings.Join(s.audio, "") +
				fmt.Sprintf("amix=inputs=%d:normalize=0[am0];", len(s.audio)) +
				"[am0]" + tail)
		}
		audioOut = "[aout]"
	}
	if s.layout && videoOut != "" {
		inner := strings.Trim(videoOut, "[]")
		// Sidebar panel: navy fill only over the video's rect, so the colored
		// box's height matches the recorded video (black letterbox above/below).
		fc.WriteString(fmt.Sprintf("[0:v]drawbox=x=%d:y=%d:w=%d:h=%d:color=0x1b2838:t=fill[vcs];",
			canvasW-sidebarW, s.chatBoxY, sidebarW, s.chatBoxH))
		fc.WriteString(fmt.Sprintf("[vcs]drawbox=x=%d:y=%d:w=%d:h=%d:color=0x5A6E82:t=1[vbg];",
			chatBoxX, s.chatBoxY, chatBoxW, s.chatBoxH))
		if s.assPath != "" {
			fc.WriteString(fmt.Sprintf("[vbg][%s]overlay=0:0[voc];[voc]subtitles=filename='%s'[vout];",
				inner, s.assPath))
		} else {
			fc.WriteString(fmt.Sprintf("[vbg][%s]overlay=0:0[vout];", inner))
		}
		videoOut = "[vout]"
		if s.outW > 0 && (s.outW != canvasW || s.outH != canvasH) {
			fc.WriteString(fmt.Sprintf("[vout]scale=%d:%d[vsc];", s.outW, s.outH))
			videoOut = "[vsc]"
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
	args = append(args, s.canvasSrc...)
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
				if doneMs < 0 {
					doneMs = 0 // AAC encoder priming reports a negative start
				}
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

// probeStreams reports which stream types a file carries plus the first video
// size. A file ffmpeg cannot read, or one carrying no stream at all, errors.
func probeStreams(ctx context.Context, ffprobe, file string) (hasVideo, hasAudio bool, w, h int, err error) {
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error", "-show_entries", "stream=codec_type,width,height",
		"-of", "csv=p=0", file)
	out, cmdErr := cmd.CombinedOutput()
	if cmdErr != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = cmdErr.Error()
		}
		return false, false, 0, 0, fmt.Errorf("ffprobe %s: %s", filepath.Base(file), msg)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "video"):
			hasVideo = true
			if w == 0 {
				fmt.Sscanf(line, "video,%dx%d", &w, &h)
			}
		case strings.HasPrefix(line, "audio"):
			hasAudio = true
		}
	}
	if !hasVideo && !hasAudio {
		return false, false, 0, 0, fmt.Errorf("ffprobe %s: no streams", filepath.Base(file))
	}
	return hasVideo, hasAudio, w, h, nil
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

// ExtractMP3 renders the audio track of src as an MP3 file.
func ExtractMP3(ctx context.Context, ffmpeg, src, dst string) error {
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", src, "-vn", "-map", "0:a:0",
		"-c:a", "libmp3lame", "-b:a", "128k", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mp3 export: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	st, err := os.Stat(dst)
	if err != nil || st.Size() < 1024 {
		return errors.New("mp3 export produced no output")
	}
	return nil
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
