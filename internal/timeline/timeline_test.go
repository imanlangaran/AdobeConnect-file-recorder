package timeline

import (
	"errors"
	"strings"
	"testing"
)

const fixture = `<?xml version="1.0" encoding="utf-8"?>
<root>
    <Signature><![CDATA[FLV]]></Signature>
    <Message time="2672159" type="data">
        <Method><![CDATA[playEvent]]></Method>
        <String><![CDATA[streamRemoved]]></String>
        <Array>
            <Object>
                <startTime><![CDATA[166]]></startTime>
                <streamName><![CDATA[/cameraVoip_0_3]]></streamName>
                <streamType><![CDATA[cameraVoip]]></streamType>
            </Object>
        </Array>
    </Message>
    <Message time="4956055" type="data">
        <Method><![CDATA[playEvent]]></Method>
        <String><![CDATA[streamRemoved]]></String>
        <Array>
            <Object>
                <startTime><![CDATA[0]]></startTime>
                <streamName><![CDATA[/screenshare_1_4]]></streamName>
                <streamType><![CDATA[screenshare]]></streamType>
            </Object>
        </Array>
    </Message>
    <Message time="4956055" type="data">
        <Method><![CDATA[playEvent]]></Method>
        <String><![CDATA[__stop__]]></String>
    </Message>
</root>`

func TestParse(t *testing.T) {
	tl, err := Parse(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if tl.DurationMs != 4956055 {
		t.Errorf("DurationMs = %d, want 4956055", tl.DurationMs)
	}
	if len(tl.Segments) != 2 {
		t.Fatalf("Segments = %d, want 2", len(tl.Segments))
	}
	// sorted by start: screenshare (0) first
	v := tl.Video()
	if len(v) != 1 || v[0].Name != "screenshare_1_4" || v[0].StartMs != 0 || v[0].EndMs != 4956055 {
		t.Errorf("Video() = %+v", v)
	}
	a := tl.Audio()
	if len(a) != 1 || a[0].Name != "cameraVoip_0_3" || a[0].DurationMs() != 2672159-166 {
		t.Errorf("Audio() = %+v", a)
	}
	if got := v[0].FileName(); got != "screenshare_1_4.flv" {
		t.Errorf("FileName() = %q", got)
	}
}

// A stream that is still live when the recording stops never gets a
// streamRemoved event — real archives do this after re-publishing a stream
// mid-recording (it must then span [startTime, __stop__]).
func TestParseClosesStreamsStillLiveAtStop(t *testing.T) {
	const s = `<?xml version="1.0" encoding="utf-8"?>
<root>
    <Message time="2674000" type="data">
        <Method><![CDATA[playEvent]]></Method>
        <String><![CDATA[streamAdded]]></String>
        <Array>
            <Object>
                <startTime><![CDATA[2676217]]></startTime>
                <streamName><![CDATA[/cameraVoip_0_6]]></streamName>
                <streamType><![CDATA[cameraVoip]]></streamType>
            </Object>
        </Array>
    </Message>
    <Message time="4956055" type="data">
        <Method><![CDATA[playEvent]]></Method>
        <String><![CDATA[__stop__]]></String>
    </Message>
</root>`
	tl, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	a := tl.Audio()
	if len(a) != 1 || a[0].Name != "cameraVoip_0_6" ||
		a[0].StartMs != 2676217 || a[0].EndMs != 4956055 {
		t.Errorf("live-at-stop stream = %+v, want cameraVoip_0_6 [2676217,4956055]", a)
	}
}

func TestParseNoStopFallsBackToMaxEnd(t *testing.T) {
	s := strings.ReplaceAll(fixture, "<String><![CDATA[__stop__]]></String>", "<String><![CDATA[other]]></String>")
	tl, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	if tl.DurationMs != 4956055 {
		t.Errorf("fallback DurationMs = %d, want 4956055", tl.DurationMs)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse(strings.NewReader("not xml")); !errors.Is(err, ErrInvalidTimeline) {
		t.Errorf("err = %v, want ErrInvalidTimeline", err)
	}
	if _, err := Parse(strings.NewReader("<root></root>")); !errors.Is(err, ErrInvalidTimeline) {
		t.Errorf("empty root err = %v, want ErrInvalidTimeline", err)
	}
}
