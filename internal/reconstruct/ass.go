package reconstruct

import (
	"fmt"
	"strings"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/assets"
)

// Layout geometry (default output): 1280x720 canvas, content box on the left,
// 320px sidebar on the right hosting the attendee list + chat replay —
// approximating the presented Adobe Connect layout.
const (
	canvasW    = 1280
	canvasH    = 720
	sidebarW   = 320
	chatStayMs = 25000 // max visibility per chat message
	maxNames   = 40
	maxChatLen = 600
)

// contentBox returns the width/height of the main content area (left of sidebar).
func contentBox() (int, int) { return canvasW - sidebarW, canvasH }

// BuildASS renders chat replay + attendee list as an ASS subtitle script.
// Sidebar: attendees pinned top; chat messages appear at their timestamp
// (one at a time, up to chatStayMs or until the next message).
func BuildASS(chat []assets.ChatMessage, attendees []string, totalMs int64) string {
	var b strings.Builder
	b.WriteString("[Script Info]\nScriptType: v4.00+\n")
	fmt.Fprintf(&b, "PlayResX: %d\nPlayResY: %d\nWrapStyle: 0\n\n", canvasW, canvasH)
	b.WriteString("[V4+ Styles]\n")
	b.WriteString("Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n")
	b.WriteString("Style: Attendees,Sans,15,&H00E8E8E8,&H00E8E8E8,&H00332211,&H80000000,0,0,0,0,100,100,0,0,1,1,0,7,970,8,12,1\n")
	b.WriteString("Style: Chat,Sans,18,&H00FFFFFF,&H00FFFFFF,&H00332211,&H80000000,0,0,0,0,100,100,0,0,1,1,0,7,970,8,280,1\n")
	b.WriteString("\n[Events]\n")
	b.WriteString("Format: Layer, Start, End, Style, Name, MarginL, MarginR, Effect, Text\n")

	if len(attendees) > 0 {
		names := attendees
		if len(names) > maxNames {
			names = append(append([]string{}, names[:maxNames]...), fmt.Sprintf("… +%d more", len(attendees)-maxNames))
		}
		text := assEscape(fmt.Sprintf("Attendees (%d)", len(attendees))) + `\N` +
			strings.Join(escAll(names), `\N`)
		fmt.Fprintf(&b, "Dialogue: 0,%s,%s,Attendees,,0,0,0,%s\n",
			assTime(0), assTime(totalMs), text)
	}

	for i, m := range chat {
		start := m.TimeMs
		if start < 0 || start >= totalMs {
			continue
		}
		end := start + chatStayMs
		if i+1 < len(chat) && chat[i+1].TimeMs > start && chat[i+1].TimeMs < end {
			end = chat[i+1].TimeMs // hand over to the next message, never overlap
		}
		if end > totalMs {
			end = totalMs
		}
		if end <= start {
			continue
		}
		text := assEscape(m.From + ": " + m.Text)
		if len([]rune(text)) > maxChatLen {
			text = string([]rune(text)[:maxChatLen]) + "…"
		}
		fmt.Fprintf(&b, "Dialogue: 0,%s,%s,Chat,,0,0,0,%s\n",
			assTime(start), assTime(end), text)
	}
	return b.String()
}

// assTime formats ms as H:MM:SS.cc (ASS timestamp).
func assTime(ms int64) string {
	cs := ms / 10 // centiseconds
	return fmt.Sprintf("%d:%02d:%02d.%02d", cs/360000, cs/6000%60, cs/100%60, cs%100)
}

// assEscape neutralizes ASS markup and flattens newlines.
func assEscape(s string) string {
	s = strings.NewReplacer("{", "(", "}", ")", "\n", " ", "\r", " ", "\t", " ").Replace(s)
	return strings.TrimSpace(s)
}

func escAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = assEscape(s)
	}
	return out
}
