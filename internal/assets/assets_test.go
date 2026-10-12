package assets

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractFlattensAndIsSafe(t *testing.T) {
	zp := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(zp)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range map[string]string{
		"mainstream.xml":   "<root/>",
		"nested/dir/x.flv": "FLV",
		"../../evil.txt":   "nope",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()

	dir := filepath.Join(t.TempDir(), "out")
	var lastDone, lastTotal int64
	if err := Extract(zp, dir, func(d, t int64) { lastDone, lastTotal = d, t }); err != nil {
		t.Fatal(err)
	}
	if lastDone != lastTotal || lastTotal != int64(len("<root/>")+len("FLV")+len("nope")) {
		t.Errorf("progress final = (%d, %d), want total %d", lastDone, lastTotal,
			int64(len("<root/>")+len("FLV")+len("nope")))
	}
	for _, n := range []string{"mainstream.xml", "x.flv", "evil.txt"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("missing flat member %s: %v", n, err)
		}
	}
	// nothing written outside dir
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.txt")); err == nil {
		t.Error("path traversal escaped target dir")
	}
}

func TestExtractRejectsJunk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "junk.zip")
	os.WriteFile(p, []byte("<html>login</html>"), 0o644)
	if err := Extract(p, t.TempDir(), nil); err == nil {
		t.Fatal("junk file should not extract")
	}
}

func TestFindHelpers(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ftchat0.xml"), []byte("x"), 0o644)
	if FindPrefix(dir, "ftchat") == "" {
		t.Error("FindPrefix ftchat")
	}
	if Find(dir, "missing.xml") != "" {
		t.Error("Find should return empty for missing")
	}
	if Find(dir, "ftchat0.xml") == "" {
		t.Error("Find existing")
	}
}

const chatFixture = `<root>
    <Message time="617705" type="data">
        <Method><![CDATA[playEvent]]></Method>
        <Object>
            <name><![CDATA[playEvent]]></name>
            <time><![CDATA[617705]]></time>
        </Object>
        <String><![CDATA[sendMessage6]]></String>
        <Object>
            <color><![CDATA[Default]]></color>
            <fromName><![CDATA[Kourosh Ahmadzadeh Ataei]]></fromName>
            <fromPID><![CDATA[44883]]></fromPID>
            <text><![CDATA[ostad id kanal telegram mizarid baramun?]]></text>
            <toName><![CDATA[]]></toName>
        </Object>
    </Message>
    <Message time="242439" type="data">
        <Method><![CDATA[playEvent]]></Method>
        <String><![CDATA[playEvent]]></String>
        <Object>
            <fromName><![CDATA[ignored]]></fromName>
            <text><![CDATA[not a chat msg]]></text>
        </Object>
    </Message>
</root>`

func TestParseChat(t *testing.T) {
	chat, err := ParseChat(strings.NewReader(chatFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(chat) != 1 {
		t.Fatalf("len = %d, want 1", len(chat))
	}
	m := chat[0]
	if m.From != "Kourosh Ahmadzadeh Ataei" || m.TimeMs != 617705 {
		t.Errorf("msg = %+v", m)
	}
	if !strings.Contains(m.Format(), "[00:10:17] Kourosh") {
		t.Errorf("Format() = %q", m.Format())
	}
}

func TestParseChatSections(t *testing.T) {
	in := `<section type="chat" position="24243">
<content>man yadgiri mashin dashtam</content>
</section>
<section type="chat" position="617671">
<content>ostad id kanal telegram mizarid baramun?</content>
</section>
<section type="slide" position="9"><content>x</content></section>`
	chat, err := ParseChatSections(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(chat) != 2 || chat[0].TimeMs != 24243 || chat[1].From != "unknown" {
		t.Errorf("chat = %+v", chat)
	}
}

func TestParseAttendees(t *testing.T) {
	p := filepath.Join(t.TempDir(), "indexstream.xml")
	os.WriteFile(p, []byte(`
<fullName><![CDATA[Hadi Sadoghi Yazdi]]></fullName>
<fullName><![CDATA[Hadi Sadoghi Yazdi]]></fullName>
<fullName><![CDATA[]]></fullName>
<fullName><![CDATA[Mohammadreza Shadloo]]></fullName>`), 0o644)
	names, err := ParseAttendees(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "Hadi Sadoghi Yazdi" || names[1] != "Mohammadreza Shadloo" {
		t.Errorf("names = %v", names)
	}
}
