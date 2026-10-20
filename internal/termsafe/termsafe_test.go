package termsafe

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func filter(chunks ...string) (string, int) {
	var out bytes.Buffer
	w := NewWriter(&out)
	for _, c := range chunks {
		w.Write([]byte(c))
	}
	w.Flush()
	return out.String(), w.Removed()
}

func TestFilter(t *testing.T) {
	cases := []struct {
		name, in, want string
		removed        int
	}{
		{"plain text", "hello\tworld\r\n", "hello\tworld\r\n", 0},
		{"utf-8", "héllo ✓ 日本 🙂", "héllo ✓ 日本 🙂", 0},
		{"colors kept", "\x1b[1;31mred\x1b[0m", "\x1b[1;31mred\x1b[0m", 0},
		{"256/truecolor kept", "\x1b[38;5;208mx\x1b[38:2::1:2:3my", "\x1b[38;5;208mx\x1b[38:2::1:2:3my", 0},
		{"clipboard write (OSC 52)", "a\x1b]52;c;ZXZpbA==\x07b", "ab", 1},
		{"title with ST", "a\x1b]0;pwned\x1b\\b", "ab", 1},
		{"hyperlink", "\x1b]8;;http://evil\x1b\\click\x1b]8;;\x1b\\", "click", 2},
		{"cursor up (hide lines)", "warning\n\x1b[1A\x1b[2Kfine", "warning\nfine", 2},
		{"private mode (alt screen)", "\x1b[?1049hx", "x", 1},
		{"device status query", "\x1b[6n", "", 1},
		{"terminal reset", "\x1bcx", "x", 1},
		{"DCS", "\x1bPq#0;2;0;0;0\x1b\\x", "x", 1},
		{"8-bit CSI as UTF-8 (U+009B)", "a\u009b31mb", "a31mb", 1},
		{"raw high byte", "a\x9bb", "a�b", 0},
		{"bell and shift-out", "a\x07\x0eb", "ab", 2},
		{"DEL", "a\x7fb", "ab", 1},
		{"SGR with private marker", "\x1b[?31m", "", 1},
		{"CSI interrupted by newline", "\x1b[31\nx", "\nx", 1},
		{"unterminated string at end", "a\x1b]0;title", "a", 1},
	}
	for _, c := range cases {
		got, removed := filter(c.in)
		if got != c.want || removed != c.removed {
			t.Errorf("%s: got %q (removed %d), want %q (removed %d)", c.name, got, removed, c.want, c.removed)
		}
	}
}

// Sequences and characters split across writes, byte by byte.
func TestSplitAcrossWrites(t *testing.T) {
	in := "x\x1b]52;c;ZXZpbA==\x1b\\y\x1b[32mgreen\x1b[0m 🙂 \x1b[2Jz"
	var chunks []string
	for i := range len(in) {
		chunks = append(chunks, in[i:i+1])
	}
	got, _ := filter(chunks...)
	want := "xy\x1b[32mgreen\x1b[0m 🙂 z"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The safety property itself: whatever the input, the output contains no
// control character except tab/newline/CR/backspace, every ESC starts an
// SGR sequence, and there are no C1 controls.
func FuzzFilter(f *testing.F) {
	f.Add([]byte("\x1b]52;c;x\x07\x1b[31mhi\x1b[0m\u009b\x9b"))
	f.Add([]byte("\x1b[1;2;3m\x1b[?25l"))
	f.Fuzz(func(t *testing.T, in []byte) {
		got, _ := filter(string(in[:len(in)/2]), string(in[len(in)/2:]))
		checkSafe(t, got)
	})
}

func checkSafe(t *testing.T, s string) {
	t.Helper()
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			t.Fatalf("invalid UTF-8 at %d in %q", i, s)
		case r == 0x1b:
			end := strings.IndexByte(s[i:], 'm')
			if !strings.HasPrefix(s[i:], "\x1b[") || end < 0 || !sgrParams([]byte(s[i+2:i+end])) {
				t.Fatalf("non-SGR escape at %d in %q", i, s)
			}
			i += end + 1
			continue
		case r < 0x20 && r != '\t' && r != '\n' && r != '\r' && r != '\b', r == 0x7f:
			t.Fatalf("control %#x at %d in %q", r, i, s)
		case r >= 0x80 && r <= 0x9f:
			t.Fatalf("C1 control %#x at %d in %q", r, i, s)
		}
		i += size
	}
}
