package proto

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Write(TypeStdout, []byte("hello"))
	w.WriteJSON(TypeExit, Exit{Code: 3})
	f, err := ReadFrame(&buf, MaxGuestFrame)
	if err != nil || f.Type != TypeStdout || string(f.Payload) != "hello" {
		t.Fatalf("frame 1 = %+v, %v", f, err)
	}
	f, err = ReadFrame(&buf, MaxGuestFrame)
	var e Exit
	if err != nil || Decode(f, &e) != nil || e.Code != 3 {
		t.Fatalf("frame 2 = %+v %+v, %v", f, e, err)
	}
}

// A hostile guest claims a huge frame: it must be rejected before any
// allocation of that size.
func TestRejectsOversizedFrame(t *testing.T) {
	hdr := []byte{TypeStdout, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(hdr[1:], 0xffffffff)
	if _, err := ReadFrame(bytes.NewReader(hdr), MaxGuestFrame); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestRejectsTruncatedFrame(t *testing.T) {
	hdr := []byte{TypeStdout, 0, 0, 0, 10, 'a', 'b'}
	if _, err := ReadFrame(bytes.NewReader(hdr), MaxGuestFrame); err == nil {
		t.Fatal("truncated frame accepted")
	}
}

func TestCreditWriterBlocksUntilGranted(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	cw := NewCreditWriter(lockedWriter{&mu, &buf})
	chunk := make([]byte, MaxHostFrame)

	// Fill the initial window exactly.
	sent := 0
	for sent+headerSize+len(chunk) <= InitialCredit {
		if err := cw.Write(TypeStdin, chunk); err != nil {
			t.Fatal(err)
		}
		sent += headerSize + len(chunk)
	}

	done := make(chan error, 1)
	go func() { done <- cw.Write(TypeStdin, chunk) }()
	select {
	case <-done:
		t.Fatal("write exceeded the credit window")
	case <-time.After(50 * time.Millisecond):
	}
	cw.Grant(InitialCredit)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	go func() { done <- cw.Write(TypeStdin, chunk) }()
	cw.Close()
	if err := <-done; err != ErrClosed {
		// The write may have fit in the remaining credit; then it succeeded.
		if err != nil {
			t.Fatalf("write after close: %v", err)
		}
	}
	if err := cw.Write(TypeStdin, make([]byte, MaxHostFrame+1)); err == nil {
		t.Fatal("oversized host frame accepted")
	}
}

func TestGrantCannotOverflow(t *testing.T) {
	cw := NewCreditWriter(&bytes.Buffer{})
	for range 10 {
		cw.Grant(1<<31 - 1)
	}
	if cw.credit <= 0 || cw.credit > 1<<30 {
		t.Fatalf("credit = %d", cw.credit)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
