package assets

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ChatMessage is one chat entry on the recording timeline.
type ChatMessage struct {
	TimeMs int64
	From   string
	Text   string
}

// Format renders the timestamp as HH:MM:SS (recording-relative).
func (m ChatMessage) Format() string {
	d := time.Duration(m.TimeMs) * time.Millisecond
	return fmt.Sprintf("[%02d:%02d:%02d] %s: %s",
		int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60, m.From, m.Text)
}

// ParseChat reads ftchat*.xml (FLV-style event stream, verified format:
// Message{time} → String "sendMessage*" → Object{fromName,text}) and returns
// messages in time order.
func ParseChat(r io.Reader) ([]ChatMessage, error) {
	dec := xml.NewDecoder(r)
	var out []ChatMessage
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("chat parse: %v", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Message" {
			continue
		}
		var m struct {
			Time   int64    `xml:"time,attr"`
			String []string `xml:"String"`
			// Real messages carry two Objects: a playEvent marker {name,time}
			// and the payload {fromName,text}. Pick the payload.
			Objects []struct {
				FromName string `xml:"fromName"`
				Text     string `xml:"text"`
			} `xml:"Object"`
		}
		if err := dec.DecodeElement(&m, &se); err != nil {
			return nil, fmt.Errorf("chat parse: %v", err)
		}
		if len(m.Objects) == 0 {
			continue
		}
		for _, o := range m.Objects {
			text := strings.TrimSpace(o.Text)
			if text == "" {
				continue
			}
			for _, s := range m.String {
				if strings.HasPrefix(s, "sendMessage") {
					from := o.FromName
					if from == "" {
						from = "unknown"
					}
					out = append(out, ChatMessage{TimeMs: m.Time, From: from, Text: text})
					break
				}
			}
			break // payload is the object that has text
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimeMs < out[j].TimeMs })
	return out, nil
}

// ParseChatSections is the fallback source: <section type="chat" position="ms">
// <content>…</content></section> from sco_metadata.xml (no author names).
func ParseChatSections(r io.Reader) ([]ChatMessage, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`(?s)<section\s+type="chat"\s+position="(\d+)">\s*<content>(.*?)</content>`)
	var out []ChatMessage
	for _, m := range re.FindAllSubmatch(data, -1) {
		text := strings.TrimSpace(unescXML(string(m[2])))
		if text == "" {
			continue
		}
		var pos int64
		fmt.Sscan(string(m[1]), &pos)
		out = append(out, ChatMessage{TimeMs: pos, From: "unknown", Text: text})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimeMs < out[j].TimeMs })
	return out, nil
}

func unescXML(s string) string {
	r := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&apos;", "'", "&amp;", "&")
	return r.Replace(s)
}

// attendeesRe extracts full names from indexstream.xml.
var attendeesRe = regexp.MustCompile(`<fullName><!\[CDATA\[([^\]]*)\]\]></fullName>`)

// ParseAttendees extracts the de-duplicated attendee name list.
func ParseAttendees(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range attendeesRe.FindAllSubmatch(data, -1) {
		n := strings.TrimSpace(string(m[1]))
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}
