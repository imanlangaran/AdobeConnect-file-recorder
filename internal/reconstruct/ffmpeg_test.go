package reconstruct

import (
	"strings"
	"testing"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/timeline"
)

func seg(kind, name string, start, end int64) timeline.Segment {
	return timeline.Segment{Kind: kind, Name: name, StartMs: start, EndMs: end}
}

func TestBuildSpecGapsAndSlots(t *testing.T) {
	vid := []timeline.Segment{seg("screenshare", "screenshare_1_4", 1000, 5000)}
	aud := []timeline.Segment{seg("cameraVoip", "cameraVoip_0_3", 0, 4000)}
	sp := buildSpec("/raw", vid, aud, 6000, 1280, 720, 25, "av", false)

	if len(sp.video) != 3 {
		t.Fatalf("video labels = %v, want [lead seg trail]", sp.video)
	}
	if len(sp.audio) != 1 {
		t.Fatalf("audio labels = %v, want just the segment (gaps come from adelay)", sp.audio)
	}

	// leading black 1s, segment 4s (-t 4.000), trailing 1s
	var srcs []string
	for _, n := range sp.nodes {
		srcs = append(srcs, strings.Join(n.src, " ")+" | "+n.filter)
	}
	joined := strings.Join(srcs, " | ")
	for _, want := range []string{
		"d=1.000",                 // video lead gap + trailing gap (6s - 4s slot - 1s lead)
		"-t 4.000",                // video slot
		"/raw/cameraVoip_0_3.flv", // audio segment
		"adelay=0:all=1",          // placed at timeline start
		"color=c=black",
		"scale=1280:720",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("graph missing %q\n%s", want, joined)
		}
	}
	// audio padded/trimmed to the exact timeline length by the spec tail
	args, err := sp.args(Options{Preset: "veryfast", CRF: 23, Out: "o.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(args, " ")
	for _, want := range []string{"apad=whole_dur=6.000", "atrim=0:6.000", "-map [aout]"} {
		if !strings.Contains(all, want) {
			t.Errorf("audio tail missing %q\n%s", want, all)
		}
	}
	if strings.Contains(all, "anullsrc") {
		t.Errorf("gap-free audio must not need silence inputs:\n%s", all)
	}
}

func TestArgsStructure(t *testing.T) {
	sp := buildSpec("/raw",
		[]timeline.Segment{seg("screenshare", "s", 0, 3000)},
		[]timeline.Segment{seg("cameraVoip", "c", 0, 3000)},
		3000, 640, 480, 25, "av", false)
	args, err := sp.args(Options{Preset: "veryfast", CRF: 23, Out: "/out/r.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(args, " ")
	for _, want := range []string{
		"ffmpeg -hide_banner", "-progress pipe:1", "-n",
		"-filter_complex", "fps=25,format=yuv420p", "aresample=48000",
		"[0:v]scale=640:480", "[1:a]aresample=48000", // input-index refs, output labels [v0]/[a1]
		"adelay=0:all=1", "apad=whole_dur=3.000", "atrim=0:3.000",
		"-map [v0]", "-map [aout]", // single video node direct; audio through placement tail
		"-c:v libx264", "-crf 23", "-c:a aac", "-movflags +faststart",
		"/out/r.mp4",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("args missing %q\nargs: %s", want, all)
		}
	}
	if strings.Contains(all, "concat=") {
		t.Errorf("single-segment graph should not need concat:\n%s", all)
	}
	if args[len(args)-1] != "/out/r.mp4" {
		t.Errorf("output must be last arg, got %q", args[len(args)-1])
	}
}

func TestArgsMultiSegmentConcat(t *testing.T) {
	sp := buildSpec("/raw",
		[]timeline.Segment{
			seg("screenshare", "s1", 0, 3000),
			seg("screenshare", "s2", 4000, 7000),
		},
		[]timeline.Segment{seg("cameraVoip", "c", 0, 7000)},
		8000, 640, 480, 25, "av", false)
	args, err := sp.args(Options{Preset: "veryfast", CRF: 23, Out: "o.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(args, " ")
	if !strings.Contains(all, "concat=n=4:v=1:a=0[vo]") {
		t.Errorf("want video concat of 4 (seg, gap, seg, trailing gap): %s", all)
	}
	// audio: one placed segment, padded — no concat, no mix needed
	for _, want := range []string{"adelay=0:all=1", "apad=whole_dur=8.000", "atrim=0:8.000"} {
		if !strings.Contains(all, want) {
			t.Errorf("audio placement missing %q: %s", want, all)
		}
	}
	if strings.Contains(all, "concat=n=") && strings.Contains(all, ":v=0:a=1") {
		t.Errorf("audio must not be concatenated: %s", all)
	}
}

func TestAudioPlacementOverlap(t *testing.T) {
	// two publishers live at the same time → segments overlap; both must play
	// at their timeline positions instead of being strung together
	sp := buildSpec("/raw",
		[]timeline.Segment{seg("screenshare", "s", 0, 6000)},
		[]timeline.Segment{
			seg("cameraVoip", "c1", 0, 4000),
			seg("cameraVoip", "c2", 3000, 6000),
		},
		6000, 640, 480, 25, "av", false)
	args, err := sp.args(Options{Preset: "veryfast", CRF: 23, Out: "o.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(args, " ")
	for _, want := range []string{
		"adelay=0:all=1", "adelay=3000:all=1",
		"amix=inputs=2:normalize=0",
		"apad=whole_dur=6.000",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q\n%s", want, all)
		}
	}
	if strings.Contains(all, "anullsrc") {
		t.Errorf("overlapping audio needs no silence inputs: %s", all)
	}
}

func TestArgsAudioOnly(t *testing.T) {
	sp := buildSpec("/raw", nil,
		[]timeline.Segment{seg("cameraVoip", "c", 0, 3000)},
		3000, 1280, 720, 25, "audio-only", false)
	args, err := sp.args(Options{Preset: "veryfast", CRF: 23, Out: "o.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(args, " ")
	if strings.Contains(all, "libx264") || strings.Contains(all, "black") {
		t.Errorf("audio-only must not encode video: %s", all)
	}
}

func TestProgressOutTime(t *testing.T) {
	if s, ok := progressOutTime("out_time_us=2500000"); !ok || s != 2.5 {
		t.Errorf("out_time_us: %v %v", s, ok)
	}
	if s, ok := progressOutTime("out_time_ms=2500000"); !ok || s != 2.5 {
		t.Errorf("out_time_ms (µs quirk): %v %v", s, ok)
	}
	if s, ok := progressOutTime("out_time=00:01:30.500000"); !ok || s != 90.5 {
		t.Errorf("out_time: %v %v", s, ok)
	}
	if _, ok := progressOutTime("progress=continue"); ok {
		t.Error("non-time key should not match")
	}
}

func TestBuildSpecLayoutOffsetsInputs(t *testing.T) {
	sp := buildSpec("/raw",
		[]timeline.Segment{seg("screenshare", "s", 0, 3000)},
		[]timeline.Segment{seg("cameraVoip", "c", 0, 3000)},
		3000, 960, 720, 25, "av", true)
	if sp.inputOffset != 1 {
		t.Errorf("inputOffset = %d, want 1 (canvas at input 0)", sp.inputOffset)
	}
	// node filters must reference canvas-shifted input indices
	if !strings.Contains(sp.nodes[0].filter, "[1:v]") {
		t.Errorf("video filter refs wrong input: %s", sp.nodes[0].filter)
	}
	for _, n := range sp.nodes {
		if strings.HasPrefix(n.filter, "[0:") {
			t.Errorf("node filter uses reserved input 0: %s", n.filter)
		}
	}
}

func TestArgsLayoutComposition(t *testing.T) {
	sp := buildSpec("/raw",
		[]timeline.Segment{seg("screenshare", "s", 0, 3000)},
		[]timeline.Segment{seg("cameraVoip", "c", 0, 3000)},
		3000, 960, 720, 25, "av", true)
	sp.layout = true
	sp.assPath = "/raw/layout.ass"
	sp.chatBoxY, sp.chatBoxH = 0, 720 // newPanel(0, canvasH, …): box == video rect
	sp.canvasSrc = []string{"-f", "lavfi", "-i", "color=c=black:s=1280x720:r=25:d=3.000"}
	args, err := sp.args(Options{Preset: "veryfast", CRF: 23, Out: "o.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(args, " ")
	for _, want := range []string{
		"color=c=black:s=1280x720", // canvas input (black; navy panel drawn only over video rect)
		"[0:v]drawbox=x=960:y=0:w=320:h=720:color=0x1b2838:t=fill[vcs];" + // navy sidebar = video's height
			"[vcs]drawbox=x=966:y=0:w=310:h=720:color=0x5A6E82:t=1[vbg];" + // border box
			"[vbg][v0]overlay=0:0", // content composited over panel
		"subtitles=filename='/raw/layout.ass'",
		"-map [vout]",
		"scale=960:720", // content box left of 320px sidebar
	} {
		if !strings.Contains(all, want) {
			t.Errorf("layout args missing %q\nargs: %s", want, all)
		}
	}
	// canvas must come before segment inputs
	ci := strings.Index(all, "color=c=black")
	si := strings.Index(all, "/raw/s.flv")
	if ci < 0 || si < 0 || ci > si {
		t.Errorf("canvas input must precede segments (canvas=%d seg=%d)", ci, si)
	}
}

func TestArgsLayoutScaleTail(t *testing.T) {
	// --resolution 480p: layout composes at 1280x720, then scales once at the end
	sp := buildSpec("/raw",
		[]timeline.Segment{seg("screenshare", "s", 0, 3000)},
		[]timeline.Segment{seg("cameraVoip", "c", 0, 3000)},
		3000, 960, 720, 25, "av", true)
	sp.layout = true
	sp.assPath = "/raw/layout.ass"
	sp.chatBoxY, sp.chatBoxH = 0, 720
	sp.canvasSrc = []string{"-f", "lavfi", "-i", "color=c=black:s=1280x720:r=25:d=3.000"}
	sp.outW, sp.outH = 854, 480
	args, err := sp.args(Options{Preset: "veryfast", CRF: 23, Out: "o.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(args, " ")
	for _, want := range []string{
		"[vout]scale=854:480[vsc]", // downscale after overlay+subtitles
		"-map [vsc]",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("scale tail missing %q\nargs: %s", want, all)
		}
	}
	if strings.Contains(all, "-map [vout]") {
		t.Errorf("must encode the scaled stream, not the pre-scale one: %s", all)
	}
}
