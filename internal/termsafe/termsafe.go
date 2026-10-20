// Package termsafe filters untrusted program output before it reaches a
// terminal.
//
// Terminals act on control sequences embedded in the text they display.
// Output from a sandboxed program could use them to write the clipboard
// (OSC 52), change the window title, rewrite or hide earlier lines (for
// example a ksar warning) with cursor movement, reset or reconfigure the
// terminal, or exploit bugs in a terminal's escape-sequence parser.
//
// Writer passes through text, UTF-8, tab, newline, carriage return,
// backspace and SGR color/style sequences (ESC [ ... m), and removes
// everything else. It is a streaming state machine: sequences split
// across Write calls are handled correctly.
package termsafe

import (
	"io"
	"unicode/utf8"
)

const (
	maxCSI    = 64       // longest SGR sequence passed through
	maxString = 64 << 10 // an unterminated OSC/DCS/... string ends here
)

type state int

const (
	ground    state = iota
	escape          // after ESC
	csi             // inside ESC [ ...
	str             // inside OSC/DCS/APC/PM/SOS, until BEL or ST
	strEscape       // ESC inside a string (maybe ST = ESC \)
)

// Writer filters what is written to it and forwards the safe part to W.
// It is not safe for concurrent use.
type Writer struct {
	w       io.Writer
	st      state
	seq     []byte // pending CSI sequence
	strLen  int
	utf     []byte // pending partial UTF-8 sequence
	removed int
	out     []byte
}

// NewWriter returns a Writer forwarding filtered output to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Removed returns how many control sequences or characters were dropped.
func (t *Writer) Removed() int { return t.removed }

// Write filters p. It always reports len(p) consumed unless the
// underlying writer fails.
func (t *Writer) Write(p []byte) (int, error) {
	t.out = t.out[:0]
	for _, b := range p {
		t.feed(b)
	}
	if len(t.out) > 0 {
		if _, err := t.w.Write(t.out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (t *Writer) drop() { t.removed++ }

func (t *Writer) feed(b byte) {
	switch t.st {
	case ground:
		t.ground(b)
	case escape:
		switch b {
		case '[':
			t.st, t.seq = csi, append(t.seq[:0], 0x1b, '[')
		case ']', 'P', '_', '^', 'X': // OSC, DCS, APC, PM, SOS
			t.st, t.strLen = str, 0
			t.drop()
		case 0x1b:
			t.drop() // ESC ESC: drop the first, stay in escape
		default:
			// Two-byte escapes (reset, save/restore cursor, charsets, ...).
			t.st = ground
			t.drop()
		}
	case csi:
		switch {
		case b >= 0x30 && b <= 0x3f, b >= 0x20 && b <= 0x2f: // params, intermediates
			if len(t.seq) >= maxCSI {
				t.st = ground
				t.drop()
				return
			}
			t.seq = append(t.seq, b)
		case b >= 0x40 && b <= 0x7e: // final byte
			t.st = ground
			if b == 'm' && sgrParams(t.seq[2:]) {
				t.out = append(t.out, t.seq...)
				t.out = append(t.out, 'm')
			} else {
				t.drop()
			}
		default:
			// Malformed: abandon the sequence, then treat b as fresh input.
			t.st = ground
			t.drop()
			t.ground(b)
		}
	case str:
		t.strLen++
		switch {
		case b == 0x07: // BEL terminates OSC
			t.st = ground
		case b == 0x1b:
			t.st = strEscape
		case t.strLen > maxString:
			t.st = ground
		}
	case strEscape:
		switch b {
		case '\\': // ST
			t.st = ground
		case 0x1b:
		default:
			t.st = str
		}
	}
}

func (t *Writer) ground(b byte) {
	if len(t.utf) > 0 {
		t.utf8(b)
		return
	}
	switch {
	case b == 0x1b:
		t.st = escape
	case b == '\t', b == '\n', b == '\r', b == '\b':
		t.out = append(t.out, b)
	case b < 0x20, b == 0x7f: // other C0 controls (BEL, SO/SI, ...) and DEL
		t.drop()
	case b < 0x80:
		t.out = append(t.out, b)
	default:
		t.utf8(b)
	}
}

// utf8 collects a multi-byte UTF-8 character. Invalid bytes become U+FFFD
// (raw high bytes could be read as 8-bit C1 controls by some terminals),
// and the C1 control characters U+0080-U+009F are dropped.
func (t *Writer) utf8(b byte) {
	if len(t.utf) > 0 && (b&0xc0 != 0x80) {
		// Expected a continuation byte: the pending sequence is invalid.
		t.utf = t.utf[:0]
		t.out = utf8.AppendRune(t.out, utf8.RuneError)
		t.ground(b)
		return
	}
	t.utf = append(t.utf, b)
	if !utf8.FullRune(t.utf) {
		if len(t.utf) >= utf8.UTFMax {
			t.utf = t.utf[:0]
			t.out = utf8.AppendRune(t.out, utf8.RuneError)
		}
		return
	}
	r, size := utf8.DecodeRune(t.utf)
	switch {
	case r == utf8.RuneError && size <= 1:
		t.out = utf8.AppendRune(t.out, utf8.RuneError)
	case r >= 0x80 && r <= 0x9f:
		t.drop()
	default:
		t.out = append(t.out, t.utf[:size]...)
	}
	t.utf = t.utf[:0]
}

// Flush emits a pending partial UTF-8 character as U+FFFD at the end of
// the stream. Unfinished escape sequences are dropped.
func (t *Writer) Flush() error {
	if len(t.utf) > 0 {
		t.utf = t.utf[:0]
		if _, err := t.w.Write([]byte(string(utf8.RuneError))); err != nil {
			return err
		}
	}
	// A pending ESC or CSI was never emitted or counted; strings were
	// counted when they started.
	if t.st == escape || t.st == csi {
		t.drop()
	}
	t.st = ground
	return nil
}

// sgrParams reports whether params are only digits and separators, i.e.
// a plain SGR (color/style) sequence.
func sgrParams(params []byte) bool {
	for _, c := range params {
		if (c < '0' || c > '9') && c != ';' && c != ':' {
			return false
		}
	}
	return true
}
