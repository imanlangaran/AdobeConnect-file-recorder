package reconstruct

import (
	"strings"
	"testing"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/assets"
)

func TestAssTime(t *testing.T) {
	cases := map[int64]string{
		0:        "0:00:00.00",
		1500:     "0:00:01.50",
		617705:   "0:10:17.70",
		37234567: "10:20:34.56",
		4956055:  "1:22:36.05",
	}
	for ms, want := range cases {
		if got := assTime(ms); got != want {
			t.Errorf("assTime(%d) = %q, want %q", ms, got, want)
		}
	}
}

func TestBuildASS(t *testing.T) {
	chat := []assets.ChatMessage{
		{TimeMs: 1000, From: "Alice", Text: "hello {\\b}world"},
		{TimeMs: 5000, From: "Bob", Text: "second"},
		{TimeMs: 5500, From: "Carol", Text: "quick"},
		{TimeMs: 99999999, From: "Late", Text: "beyond end"}, // out of range → skipped
	}
	ass := BuildASS(chat, []string{"Alice", "Bob"}, 10000)

	if !strings.Contains(ass, "PlayResX: 1280") || !strings.Contains(ass, "PlayResY: 720") {
		t.Error("missing canvas resolution")
	}
	if !strings.Contains(ass, "Attendees (2)") || !strings.Contains(ass, `Alice\NBob`) {
		t.Errorf("attendee block wrong:\n%s", ass)
	}
	// msg1 block: only Alice; braces escaped; lives until the next message
	if !strings.Contains(ass, "Dialogue: 0,0:00:01.00,0:00:05.00,Chat,,0,0,0,Alice: hello (\\b)world") {
		t.Errorf("msg1 history block wrong:\n%s", ass)
	}
	// msg2 block: history accumulates (oldest on top, newest at the bottom)
	if !strings.Contains(ass, "0:00:05.00,0:00:05.50,Chat,,0,0,0,Alice: hello (\\b)world\\NBob: second") {
		t.Errorf("msg2 history block wrong:\n%s", ass)
	}
	// msg3 block: full history, stays until the end of the recording
	if !strings.Contains(ass, "0:00:05.50,0:00:10.00,Chat,,0,0,0,Alice: hello (\\b)world\\NBob: second\\NCarol: quick") {
		t.Errorf("msg3 history block wrong:\n%s", ass)
	}
	if strings.Contains(ass, "beyond end") {
		t.Error("out-of-range message must be skipped")
	}
	// exactly 3 chat dialogues
	if n := strings.Count(ass, ",Chat,,"); n != 3 {
		t.Errorf("chat dialogues = %d, want 3", n)
	}
	// no overlapping chat events (end <= next start)
	lines := strings.Split(ass, "\n")
	var prevEnd string
	for _, l := range lines {
		if !strings.Contains(l, ",Chat,,") {
			continue
		}
		parts := strings.SplitN(l, ",", 4)
		if prevEnd != "" && parts[2] < prevEnd {
			t.Errorf("overlap: %s starts before previous end %s", parts[2], prevEnd)
		}
		prevEnd = parts[2]
	}
}

func TestBuildASSHistoryCap(t *testing.T) {
	// many long messages: the box must never exceed maxChatLines lines, and
	// the final block must stay visible until the end of the recording
	var chat []assets.ChatMessage
	for i := 0; i < 40; i++ {
		chat = append(chat, assets.ChatMessage{
			TimeMs: int64(i+1) * 1000, From: "User", Text: strings.Repeat("x", 120),
		})
	}
	ass := BuildASS(chat, nil, 60000)
	if !strings.Contains(ass, ",0:01:00.00,Chat,,") {
		t.Error("last history block must run until the end of the recording")
	}
	var last string
	for _, l := range strings.Split(ass, "\n") {
		if strings.Contains(l, ",Chat,,") {
			last = l
		}
	}
	if n := strings.Count(last, `\N`) + 1; n > maxChatLines {
		t.Errorf("history block = %d lines, want <= %d\n%s", n, maxChatLines, last)
	}
}

func TestBuildASSEmpty(t *testing.T) {
	ass := BuildASS(nil, nil, 1000)
	if strings.Contains(ass, "Dialogue:") {
		t.Error("no data → no dialogue lines")
	}
}
