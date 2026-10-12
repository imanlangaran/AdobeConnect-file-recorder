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
	sp := buildSpec("/raw", vid, aud, 6000, 1280, 720, 25, "av")

	if len(sp.video) != 3 {
		t.Fatalf("video labels = %v, want [lead seg trail]", sp.video)
	}
	if len(sp.audio) != 2 {
		t.Fatalf("audio labels = %v, want [seg trail] (lead gap is 0 → skipped)", sp.audio)
	}

	// leading black 1s, segment 4s (-t 4.000), trailing 1s
	var srcs []string
	for _, n := range sp.nodes {
		srcs = append(srcs, strings.Join(n.src, " ")+" | "+n.filter)
	}
	joined := strings.Join(srcs, " | ")
	for _, want := range []string{
		"d=1.000",                 // video lead gap
		"-t 4.000",                // video slot
		"d=2.000",                 // video trailing gap (6-4)
		"anullsrc",                // audio silence
		"/raw/cameraVoip_0_3.flv", // audio segment
		"color=c=black",
		"scale=1280:720",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("graph missing %q\n%s", want, joined)
		}
	}
	// audio lead 0 → no 0-duration silence before segment; trailing silence 2s
	if strings.Contains(joined, "anullsrc=r=48000:cl=stereo:d=0.000") {
		t.Error("zero-duration silence node present")
	}
}

func TestArgsStructure(t *testing.T) {
	sp := buildSpec("/raw",
		[]timeline.Segment{seg("screenshare", "s", 0, 3000)},
		[]timeline.Segment{seg("cameraVoip", "c", 0, 3000)},
		3000, 640, 480, 25, "av")
	args, err := sp.args(Options{Preset: "veryfast", CRF: 23, Out: "/out/r.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(args, " ")
	for _, want := range []string{
		"ffmpeg -hide_banner", "-progress pipe:1", "-n",
		"-filter_complex", "fps=25,format=yuv420p", "aresample=48000",
		"[0:v]scale=640:480", "[1:a]aresample=48000", // input-index refs, output labels [v0]/[a1]
		"-map [v0]", "-map [a1]", // single-node: labels used directly, no concat
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
		8000, 640, 480, 25, "av")
	args, err := sp.args(Options{Preset: "veryfast", CRF: 23, Out: "o.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(args, " ")
	if !strings.Contains(all, "concat=n=4:v=1:a=0[vo]") {
		t.Errorf("want video concat of 4 (seg, gap, seg, trailing gap): %s", all)
	}
	if !strings.Contains(all, "concat=n=2:v=0:a=1[ao]") {
		t.Errorf("want audio concat of 2 (seg, trailing silence): %s", all)
	}
}

func TestArgsAudioOnly(t *testing.T) {
	sp := buildSpec("/raw", nil,
		[]timeline.Segment{seg("cameraVoip", "c", 0, 3000)},
		3000, 1280, 720, 25, "audio-only")
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
