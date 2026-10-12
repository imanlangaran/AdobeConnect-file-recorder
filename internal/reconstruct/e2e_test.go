package reconstruct

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEndToEnd builds a miniature recording (FLV segments + mainstream.xml)
// and reconstructs it. Skipped when ffmpeg/ffprobe are unavailable.
func TestEndToEnd(t *testing.T) {
	for _, b := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
	dir := t.TempDir()

	// video segment: 2s test pattern, 320x240, FLV codec (structure-real: FLV container)
	runFF(t, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10",
		"-c:v", "flv", "-pix_fmt", "yuv420p",
		filepath.Join(dir, "screenshare_1_4.flv"))
	// audio segment: 1s tone, nellymoser (matches real cameraVoip streams), audio-only FLV
	runFF(t, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:a", "nellymoser", "-ar", "22050", "-ac", "1",
		filepath.Join(dir, "cameraVoip_0_3.flv"))

	// timeline: video [0,2000] audio [500,1500] stop at 2500ms
	timelineXML := `<?xml version="1.0"?>
<root>
 <Message time="2000" type="data"><String><![CDATA[streamRemoved]]></String><Array><Object>
   <startTime><![CDATA[0]]></startTime>
   <streamName><![CDATA[/screenshare_1_4]]></streamName>
   <streamType><![CDATA[screenshare]]></streamType>
 </Object></Array></Message>
 <Message time="1500" type="data"><String><![CDATA[streamRemoved]]></String><Array><Object>
   <startTime><![CDATA[500]]></startTime>
   <streamName><![CDATA[/cameraVoip_0_3]]></streamName>
   <streamType><![CDATA[cameraVoip]]></streamType>
 </Object></Array></Message>
 <Message time="2500" type="data"><String><![CDATA[__stop__]]></String></Message>
</root>`
	if err := os.WriteFile(filepath.Join(dir, "mainstream.xml"), []byte(timelineXML), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "recording.mp4")
	var lastMs int64
	res, err := Run(context.Background(), Options{
		Dir: dir, Out: out, Overwrite: true,
		Progress: func(d, total int64) {
			if d < lastMs {
				t.Errorf("progress went backwards: %d < %d", d, lastMs)
			}
			lastMs = d
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "av" || res.VideoSegments != 1 || res.AudioSegments != 1 {
		t.Errorf("result = %+v", res)
	}

	durMs, err := ProbeDuration(context.Background(), "ffprobe", out)
	if err != nil {
		t.Fatal(err)
	}
	// expected 2500ms; allow 500ms tolerance (encoder/concat rounding)
	if durMs < 2000 || durMs > 3000 {
		t.Errorf("duration = %dms, want ~2500ms ±500", durMs)
	}

	// output must be real MP4 with video + audio streams
	probe := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "stream=codec_type,codec_name",
		"-of", "compact", out)
	pout, err := probe.Output()
	if err != nil {
		t.Fatal(err)
	}
	ps := string(pout)
	for _, want := range []string{"video", "h264", "audio", "aac"} {
		if !strings.Contains(ps, want) {
			t.Errorf("output streams missing %q:\n%s", want, ps)
		}
	}
}

func runFF(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
