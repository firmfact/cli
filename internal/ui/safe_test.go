package ui

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSafeTextEscapesTerminalControls(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"OSC 52 clipboard write", "ok\x1b]52;c;cm0gLXJmIH4=\x07done", `ok\u001b]52;c;cm0gLXJmIH4=\u0007done`},
		{"OSC 8 hidden link", "\x1b]8;;https://evil.example\x1b\\Acme\x1b]8;;\x1b\\", `\u001b]8;;https://evil.example\u001b\Acme\u001b]8;;\u001b\`},
		{"CSI erase line", "real\x1b[2Kfake", `real\u001b[2Kfake`},
		{"C1 CSI as a character", "a\u009b2Kb", `a\u009b2Kb`},
		{"C1 CSI as a lone byte", "a\x9b2Kb", `a\x9b2Kb`},
		{"DEL and a carriage return", "x\x7fy\rz", `x\u007fy\u000dz`},
		{"newline and tab stay", "line one\n\tline two", "line one\n\tline two"},
		{"CRLF becomes a newline", "one\r\ntwo", "one\ntwo"},
		{"plain text is unchanged", "Kosten 1.200 € in Zürich", "Kosten 1.200 € in Zürich"},
		{"right-to-left override", "Acme\u202eknaB", `Acme\u202eknaB`},
		{"isolates and marks", "a\u2066b\u2069c\u200fd\u061ce", `a\u2066b\u2069c\u200fd\u061ce`},
		{"zero-width space and BOM", "Ac\u200bme\ufeff", `Ac\u200bme\ufeff`},
		{"joiners stay for scripts and emoji", "می\u200cخواهم 👨\u200d👩\u200d👧", "می\u200cخواهم 👨\u200d👩\u200d👧"},
	}
	for _, c := range cases {
		got := SafeText(c.in)
		if got != c.want {
			t.Errorf("%s: SafeText(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		if strings.ContainsAny(got, "\x1b\x07\x7f\u009b\u202e\u2066\u200b\ufeff") || strings.Contains(got, "\x9b") {
			t.Errorf("%s: control characters left in %q", c.name, got)
		}
		if again := SafeText(got); again != got {
			t.Errorf("%s: SafeText is not idempotent: %q then %q", c.name, got, again)
		}
	}
}

// A table cell or a progress line cannot be split or re-aligned from the
// server's side.
func TestSafeLineKeepsOneLine(t *testing.T) {
	got := SafeLine("Acme\nerror: fake\tcol\r\x1b[2K")
	if want := `Acme error: fake col \u001b[2K`; got != want {
		t.Errorf("SafeLine = %q, want %q", got, want)
	}
	if got := SafeLine("Bank BV"); got != "Bank BV" {
		t.Errorf("plain text changed: %q", got)
	}
}

func TestSafeJSONEscapesWhatEncodingJSONLeaves(t *testing.T) {
	value := map[string]string{"name": "a\x1b[2K\x7f\u009bb\u202ec\u200bd", "plain": "€ ok\u200d"}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	safe := SafeJSON(raw)
	if strings.ContainsAny(string(safe), "\x1b\x7f\u009b\u202e\u200b") {
		t.Errorf("control characters left in %q", safe)
	}
	var back map[string]string
	if err := json.Unmarshal(safe, &back); err != nil {
		t.Fatalf("no longer JSON: %v", err)
	}
	if back["name"] != value["name"] || back["plain"] != value["plain"] {
		t.Errorf("value changed: %q", back)
	}
	clean := []byte(`{"a":"b"}`)
	if got := SafeJSON(clean); &got[0] != &clean[0] {
		t.Error("clean JSON should be passed through without a copy")
	}
}

// Indented JSON keeps its layout: the newlines and tabs between values are
// not the server's.
func TestSafeJSONKeepsLayout(t *testing.T) {
	raw := []byte("{\n\t\"name\": \"Acme\u202e\"\n}")
	if got, want := string(SafeJSON(raw)), "{\n\t\"name\": \"Acme\\u202e\"\n}"; got != want {
		t.Errorf("SafeJSON = %q, want %q", got, want)
	}
}
