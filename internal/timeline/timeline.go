// Package timeline parses the mainstream.xml master timeline from an
// Adobe Connect Standard AV recording archive.
//
// Format (verified against a real archive from webinar5.um.ac.ir):
//
//	<Message time="2672159" type="data">
//	  <Method>playEvent</Method>
//	  <String>streamRemoved</String>
//	  <Array><Object>
//	    <startTime>166</startTime>
//	    <streamName><![CDATA[/cameraVoip_0_3]]></streamName>
//	    <streamType>cameraVoip</streamType>
//	  </Object></Array>
//	</Message>
//
// A segment spans [startTime, Message@time]. Total duration is given by the
// Message containing <String>__stop__</String>.
package timeline

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrInvalidTimeline indicates mainstream.xml is missing or malformed.
var ErrInvalidTimeline = errors.New("invalid recording timeline")

// Segment is one continuous stream run within the recording.
type Segment struct {
	Kind    string // "cameraVoip" (audio) or "screenshare" (video)
	Name    string // stream base name, e.g. "cameraVoip_0_3"
	StartMs int64
	EndMs   int64
}

// DurationMs returns the segment length in milliseconds.
func (s Segment) DurationMs() int64 { return s.EndMs - s.StartMs }

// Timeline is the parsed recording timeline.
type Timeline struct {
	DurationMs int64
	Segments   []Segment
}

// Video returns screenshare segments in time order.
func (t Timeline) Video() []Segment { return filter(t.Segments, "screenshare") }

// Audio returns cameraVoip segments in time order.
func (t Timeline) Audio() []Segment { return filter(t.Segments, "cameraVoip") }

func filter(segs []Segment, kind string) []Segment {
	var out []Segment
	for _, s := range segs {
		if s.Kind == kind && s.DurationMs() > 0 {
			out = append(out, s)
		}
	}
	return out
}

type xmlMessage struct {
	Time   int64    `xml:"time,attr"`
	String []string `xml:"String"`
	Array  struct {
		Object struct {
			StartTime  int64  `xml:"startTime"`
			StreamName string `xml:"streamName"`
			StreamType string `xml:"streamType"`
		} `xml:"Object"`
	} `xml:"Array"`
}

// Parse reads mainstream.xml and extracts the stream timeline.
func Parse(r io.Reader) (Timeline, error) {
	dec := xml.NewDecoder(r)
	var tl Timeline
	var maxEnd int64
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Timeline{}, fmt.Errorf("%w: %v", ErrInvalidTimeline, err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Message" {
			continue
		}
		var m xmlMessage
		if err := dec.DecodeElement(&m, &se); err != nil {
			return Timeline{}, fmt.Errorf("%w: %v", ErrInvalidTimeline, err)
		}
		for _, s := range m.String {
			switch {
			case s == "__stop__":
				tl.DurationMs = m.Time
			case strings.HasPrefix(s, "streamRemoved"):
				o := m.Array.Object
				if o.StreamName == "" || o.StartTime < 0 || m.Time <= o.StartTime {
					continue
				}
				name := strings.TrimPrefix(o.StreamName, "/")
				kind := o.StreamType
				if kind == "" { // older archives omit streamType; infer from name
					kind = kindFromName(name)
				}
				if kind == "" {
					continue
				}
				tl.Segments = append(tl.Segments, Segment{
					Kind: kind, Name: name, StartMs: o.StartTime, EndMs: m.Time,
				})
				if m.Time > maxEnd {
					maxEnd = m.Time
				}
			}
		}
	}
	if len(tl.Segments) == 0 {
		return Timeline{}, fmt.Errorf("%w: no stream segments found", ErrInvalidTimeline)
	}
	if tl.DurationMs == 0 {
		tl.DurationMs = maxEnd // fallback: recording stop event missing
	}
	sortSegments(tl.Segments)
	return tl, nil
}

// ParseFile is a convenience wrapper around Parse.
func ParseFile(path string) (Timeline, error) {
	f, err := os.Open(path)
	if err != nil {
		return Timeline{}, fmt.Errorf("%w: %v", ErrInvalidTimeline, err)
	}
	defer f.Close()
	return Parse(f)
}

func kindFromName(name string) string {
	base := name
	if i := strings.LastIndexByte(name, '_'); i > 0 {
		base = name[:i]
	}
	switch {
	case strings.HasPrefix(base, "cameraVoip"):
		return "cameraVoip"
	case strings.HasPrefix(base, "screenshare"):
		return "screenshare"
	}
	return ""
}

func sortSegments(segs []Segment) {
	sort.Slice(segs, func(i, j int) bool { return segs[i].StartMs < segs[j].StartMs })
}

// FileName returns the archive member filename for a segment ("x.flv").
func (s Segment) FileName() string { return filepath.Base(s.Name) + ".flv" }
