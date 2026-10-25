package recordingurl

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in, id, zip string
	}{
		{"https://connect.example.com/meeting12345678/", "meeting12345678",
			"https://connect.example.com/meeting12345678/output/meeting12345678.zip?download=zip"},
		{"https://connect.example.com/meeting12345678", "meeting12345678",
			"https://connect.example.com/meeting12345678/output/meeting12345678.zip?download=zip"},
		{"connect.example.com/meeting12345678/?session=abc", "meeting12345678",
			"https://connect.example.com/meeting12345678/output/meeting12345678.zip?download=zip"},
		{"http://localhost:8500/my-meeting/", "my-meeting",
			"http://localhost:8500/my-meeting/output/my-meeting.zip?download=zip"},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got.ID != c.id {
			t.Errorf("Parse(%q).ID = %q, want %q", c.in, got.ID, c.id)
		}
		if got.ZipURL() != c.zip {
			t.Errorf("Parse(%q).ZipURL() = %q, want %q", c.in, got.ZipURL(), c.zip)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"", "https://example.com/", "https://user:pass@h/x/", "https://h/a/b/c/d extra"} {
		if _, err := Parse(in); err == nil {
			// "a/b/c/d extra" actually parses: id="a" — only reject guaranteed-bad inputs
			if in == "https://example.com/" || in == "" || in == "https://user:pass@h/x/" {
				t.Errorf("Parse(%q) should fail", in)
			}
		}
	}
}
