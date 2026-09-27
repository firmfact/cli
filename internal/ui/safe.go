package ui

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Text from the server (tool results, notes, errors, tool descriptions,
// names) is printed on the user's terminal, and a terminal obeys the control
// sequences in what it prints: OSC 52 writes the clipboard, OSC 8 hides a
// link behind harmless text, CSI sequences move the cursor, clear lines or
// retitle the window. A compromised or spoofed server could use that to
// plant a command in the clipboard or rewrite what the user sees. So
// server-derived text goes through SafeText or SafeLine before it is
// printed, and control characters show up as visible escapes instead of
// acting. Escaping rather than dropping keeps the text honest: the user sees
// that something odd was there.
//
// Some Unicode characters rewrite what the user sees without any escape
// sequence. A terminal that lays out bidirectional text (VTE, Konsole,
// mlterm) obeys U+202E and its kind, so a vendor name holding one can make
// a table row or a "may delete" question read differently from what the
// command acts on; zero-width characters make two different names look
// alike. Those are escaped too (see escaped).

// SafeText returns s with every control character that could drive a
// terminal replaced by a visible escape such as \u001b: C0 controls other
// than tab and newline, DEL, and C1 controls (U+0080 to U+009F, among them
// the one-character CSI), and the bidirectional and invisible characters
// escaped lists. A CRLF line ending becomes a plain newline. Bytes that are
// not valid UTF-8 become \x9b style escapes, because a terminal that is not
// in UTF-8 mode reads a lone 0x9b byte as CSI.
func SafeText(s string) string { return sanitise(s, false) }

// SafeLine is SafeText for text that must stay on one line: table cells, a
// progress line redrawn in place, a name inside a sentence. Tabs, carriage
// returns and newlines become spaces, so they cannot break a table's columns
// or start a line that looks like the CLI's own output.
func SafeLine(s string) string { return sanitise(s, true) }

func sanitise(s string, oneLine bool) string {
	if isSafe(s, oneLine) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case oneLine && (r == '\t' || r == '\n' || r == '\r'):
			b.WriteByte(' ')
		case r == '\r' && strings.HasPrefix(s[i+1:], "\n"):
			// CRLF: the newline that follows is written on its own.
		case escaped(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// isSafe is the common case, checked first so clean text is not copied.
func isSafe(s string, oneLine bool) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 || escaped(r) || oneLine && (r == '\t' || r == '\n') {
			return false
		}
		i += size
	}
	return true
}

// escaped reports the characters SafeText escapes: all C0 controls but tab
// and newline, DEL, the C1 range, and the format characters invisibleOrBidi
// names.
func escaped(r rune) bool {
	return r < 0x20 && r != '\t' && r != '\n' || r == 0x7f || r >= 0x80 && r <= 0x9f || invisibleOrBidi(r)
}

// invisibleOrBidi reports the Unicode format characters (category Cf) that
// reorder text or take up no space: the bidirectional marks, embeddings,
// overrides and isolates that "Trojan Source" used (U+061C, U+200E, U+200F,
// U+202A to U+202E, U+2066 to U+2069), the zero-width space, word joiner and
// byte order mark, the invisible operators, the deprecated format controls
// and the interlinear annotations. The zero-width non-joiner and joiner
// (U+200C, U+200D) stay: Persian and the Indic scripts need them, and so do
// emoji such as a family. So do the soft hyphen, which terminals show, and
// the tag characters of flags such as Scotland's.
func invisibleOrBidi(r rune) bool {
	switch {
	case r < 0x061c:
		return false
	case r == 0x061c, r == 0x200b, r == 0x200e, r == 0x200f, r == 0xfeff:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2060 && r <= 0x2064, r >= 0x2066 && r <= 0x206f, r >= 0xfff9 && r <= 0xfffb:
		return true
	}
	return false
}

// SafeJSON escapes the characters encoding/json leaves raw in its output
// that SafeText escapes: it escapes C0 controls itself, but writes DEL, the
// C1 range (U+0080 to U+009F, as the UTF-8 pairs C2 80 to C2 9F) and the
// bidirectional and invisible characters of invisibleOrBidi unchanged, and a
// terminal acts on those. They can only occur inside JSON strings, where a
// \u escape means the same character, so the result parses to exactly the
// same value. Tabs and newlines between the values stay as they are.
func SafeJSON(raw []byte) []byte {
	if !jsonNeedsEscapes(raw) {
		return raw
	}
	out := make([]byte, 0, len(raw)+16)
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if jsonEscaped(r, size) {
			out = fmt.Appendf(out, `\u%04x`, r)
		} else {
			out = append(out, raw[i:i+size]...)
		}
		i += size
	}
	return out
}

// jsonNeedsEscapes is the common case of SafeJSON, checked first so clean
// JSON is not copied: plain ASCII but DEL needs nothing.
func jsonNeedsEscapes(raw []byte) bool {
	for i := 0; i < len(raw); {
		if raw[i] < utf8.RuneSelf {
			if raw[i] == 0x7f {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(raw[i:])
		if jsonEscaped(r, size) {
			return true
		}
		i += size
	}
	return false
}

// jsonEscaped reports a character SafeJSON escapes; a byte that is not
// valid UTF-8 (size 1) is left for the JSON decoder to refuse.
func jsonEscaped(r rune, size int) bool {
	return r == 0x7f || size > 1 && (r >= 0x80 && r <= 0x9f || invisibleOrBidi(r))
}
