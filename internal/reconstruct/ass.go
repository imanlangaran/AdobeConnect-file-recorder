package reconstruct

import (
	"fmt"
	"strings"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/assets"
)

// Layout geometry (default output): 1280x720 canvas, content box on the left,
// 320px sidebar on the right hosting the attendee list + chat replay —
// approximating the presented Adobe Connect layout. The sidebar box spans the
// recorded video's vertical rect (letterboxed content): same height as the
// video, border exactly on its top/bottom edge.
const (
	canvasW      = 1280
	canvasH      = 720
	sidebarW     = 320
	maxNames     = 40
	chatCols     = 30 // ~chars per wrapped line at 18px in the 302px-wide box
	maxMsgLines  = 2  // per-message cap: one long message must not eat the box
	maxChatLines = 19 // rolling history depth cap (lines)
	// vertical rhythm inside the video rect — line pitches measured from the
	// burned output (Sans 15 → 15px, Sans 18 → 18px per line)
	attPadTop   = 12 // attendee block top inset inside the box
	attPitch    = 15
	attGap      = 37 // attendee block → chat text
	glyphTop    = 4  // glyph offset below MarginV
	glyphDepth  = 13 // glyph height incl. descenders
	chatPitch   = 19 // pitch ceiling for fit math (measured 18)
	botSlack    = 3  // keep text this far above the box border
	minBoxLines = 6
)

// contentBox returns the width/height of the main content area (left of sidebar).
func contentBox() (int, int) { return canvasW - sidebarW, canvasH }

// fitRect mirrors ffmpeg's scale=boxW:boxH:force_original_aspect_ratio=decrease
// with a centered pad: it returns the top row and height the recorded video
// occupies inside the content box (canvas coordinates).
func fitRect(sw, sh, boxW, boxH int) (top, h int) {
	if boxW*sh <= boxH*sw { // width-limited
		h = (sh*boxW + sw/2) / sw
	} else {
		h = boxH
	}
	return (boxH - h) / 2, h
}

// panel is the sidebar geometry within the recorded video's rect
// [vTop, vTop+vH). The chat box spans the video's full height (its border
// sits exactly on the video's top/bottom edge); attendees are pinned at the
// box top, the rolling chat history fills it below.
type panel struct {
	attMarginV, chatMarginV int
	boxY, boxH              int // chat history box == video rect (ffmpeg drawbox)
	maxLines                int // rolling chat history depth
	show                    int // attendee names rendered
	trunc                   bool
}

// newPanel derives the sidebar layout for a video rect of height vH whose top
// row is vTop, from nNames attendees. The box always equals the video rect;
// letterboxed content (vH < canvasH) simply gets a shorter panel.
func newPanel(vTop, vH, nNames int) panel {
	vBot := vTop + vH
	attM := vTop + attPadTop
	// attendees may take at most this many lines (incl. header) — the chat
	// text must still get minBoxLines inside the box
	maxAtt := (vBot - 1 - botSlack - glyphTop - glyphDepth -
		(minBoxLines-1)*chatPitch - attGap - attM) / attPitch
	if maxAtt < 1 {
		maxAtt = 1 // ponytail: assumes vH ≳ 200; wider-than-~5:1 video gets cramped
	}
	nameRows := maxAtt - 1
	if nameRows > maxNames+1 {
		nameRows = maxNames + 1
	}
	show := nNames
	if show > nameRows {
		show = nameRows - 1 // reserve the last row for "… +N more"
		if show < 0 {
			show = 0
		}
	}
	nLines := 0
	if nNames > 0 {
		nLines = 1 + show // header
		if show < nNames {
			nLines++ // ellipsis row
		}
	}
	chatM := attM + nLines*attPitch + attGap
	maxLines := (vBot-1-botSlack-(chatM+glyphTop+glyphDepth))/chatPitch + 1
	if maxLines > maxChatLines {
		maxLines = maxChatLines
	}
	if maxLines < 1 {
		maxLines = 1 // degenerate rects only
	}
	return panel{
		attMarginV:  attM,
		chatMarginV: chatM,
		boxY:        vTop,
		boxH:        vH,
		maxLines:    maxLines,
		show:        show,
		trunc:       show < nNames,
	}
}

// BuildASS renders the attendee list + rolling chat history as an ASS script
// for the given sidebar panel geometry.
// Sidebar: attendees pinned top; at each chat message the box shows the most
// recent messages that fit — the last block stays until the end of the
// recording, so history never disappears.
func BuildASS(chat []assets.ChatMessage, attendees []string, totalMs int64, p panel) string {
	var b strings.Builder
	b.WriteString("[Script Info]\nScriptType: v4.00+\n")
	fmt.Fprintf(&b, "PlayResX: %d\nPlayResY: %d\nWrapStyle: 0\n\n", canvasW, canvasH)
	b.WriteString("[V4+ Styles]\n")
	b.WriteString("Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n")
	fmt.Fprintf(&b, "Style: Attendees,Sans,15,&H00E8E8E8,&H00E8E8E8,&H00332211,&H80000000,0,0,0,0,100,100,0,0,1,1,0,7,970,8,%d,1\n", p.attMarginV)
	fmt.Fprintf(&b, "Style: Chat,Sans,18,&H00FFFFFF,&H00FFFFFF,&H00332211,&H80000000,0,0,0,0,100,100,0,0,1,1,0,7,970,8,%d,1\n", p.chatMarginV)
	b.WriteString("\n[Events]\n")
	b.WriteString("Format: Layer, Start, End, Style, Name, MarginL, MarginR, Effect, Text\n")

	if len(attendees) > 0 {
		names := escAll(attendees[:p.show])
		if p.trunc {
			names = append(names, assEscape(fmt.Sprintf("… +%d more", len(attendees)-p.show)))
		}
		text := assEscape(fmt.Sprintf("Attendees (%d)", len(attendees))) + `\N` +
			strings.Join(names, `\N`)
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
			if used+n > p.maxLines {
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
