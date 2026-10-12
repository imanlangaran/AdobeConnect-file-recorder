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
	canvasW  = 1280
	canvasH  = 720
	sidebarW = 320
	maxNames = 40
	// chat history box: text top = Chat style MarginV (280), bottom inset 10px
	chatCols     = 30 // ~chars per wrapped line at 18px in the 302px-wide box
	maxMsgLines  = 2  // per-message cap: one long message must not eat the box
	maxChatLines = 19 // rolling history depth (lines that fit the box)
)

// contentBox returns the width/height of the main content area (left of sidebar).
func contentBox() (int, int) { return canvasW - sidebarW, canvasH }

// BuildASS renders the attendee list + rolling chat history as an ASS script.
// Sidebar: attendees pinned top; at each chat message the box shows the most
// recent messages that fit — the last block stays until the end of the
// recording, so history never disappears.
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

	// keep only messages that fall inside the timeline
	var msgs []assets.ChatMessage
	for _, m := range chat {
		if m.TimeMs >= 0 && m.TimeMs < totalMs {
			msgs = append(msgs, m)
		}
	}

	for i := range msgs {
		start := msgs[i].TimeMs
		end := totalMs
		if i+1 < len(msgs) {
			end = msgs[i+1].TimeMs
		}
		if end <= start {
			continue
		}
		// rolling history: accumulate newest → oldest until the box is full;
		// the block lives until the next message (last block: until the end)
		var lines []string
		used := 0
		for j := i; j >= 0; j-- {
			t := chatLine(msgs[j])
			n := (len([]rune(t)) + chatCols - 1) / chatCols
			if used+n > maxChatLines {
				break
			}
			used += n
			lines = append([]string{t}, lines...)
		}
		fmt.Fprintf(&b, "Dialogue: 0,%s,%s,Chat,,0,0,0,%s\n",
			assTime(start), assTime(end), strings.Join(lines, `\N`))
	}
	return b.String()
}

// chatLine renders one message, capped at maxMsgLines so a single long
// message cannot consume the whole history box.
func chatLine(m assets.ChatMessage) string {
	t := assEscape(m.From + ": " + m.Text)
	if r := []rune(t); len(r) > maxMsgLines*chatCols {
		t = string(r[:maxMsgLines*chatCols]) + "…"
	}
	return t
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
