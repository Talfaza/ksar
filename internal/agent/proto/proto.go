// Package proto is the wire protocol between Ksar (host) and ksar-agent
// (guest PID 1). It runs over any byte stream: a serial port today, vsock
// later.
//
// Every message is a frame:
//
//	type   uint8
//	length uint32 (big endian)
//	payload [length]byte
//
// Host-to-guest traffic is flow controlled: the guest grants credit (in
// bytes) and the host never has more unacknowledged bytes in flight than
// granted. A serial line has small kernel buffers in the guest, so without
// this, bursts from the host could overflow them and silently lose data.
// Guest-to-host traffic needs no flow control: the host buffers it.
//
// The host must treat everything the guest sends as hostile: frame sizes
// are capped and unknown or malformed frames are errors.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Version is bumped on incompatible protocol changes.
const Version = 1

// Message types.
const (
	// Guest -> host.
	TypeReady  = 0x01 // JSON Ready: agent is up
	TypeCredit = 0x02 // uint32: host may send this many more bytes
	TypeResult = 0x03 // JSON Result: outcome of a file transfer
	TypeStdout = 0x20 // raw bytes
	TypeStderr = 0x21 // raw bytes
	TypeExit   = 0x22 // JSON Exit: command finished

	// Host -> guest.
	TypeFileOpen   = 0x10 // JSON FileOpen: start writing a file
	TypeFileData   = 0x11 // raw bytes appended to the open file
	TypeFileClose  = 0x12 // finish the file; guest replies with Result
	TypeExec       = 0x13 // JSON Exec: run a command
	TypeStdin      = 0x14 // raw bytes for the command's stdin
	TypeStdinClose = 0x15 // close the command's stdin
)

const (
	headerSize = 5

	// MaxHostFrame is the largest payload the host sends. It must stay
	// well below InitialCredit.
	MaxHostFrame = 16 << 10

	// MaxGuestFrame is the largest payload either side accepts. A guest
	// sending more is misbehaving.
	MaxGuestFrame = 64 << 10

	// InitialCredit is how many bytes the host may send before the guest
	// grants more. The Linux tty layer buffers up to ~68 KiB per port.
	InitialCredit = 32 << 10
)

// Ready is sent by the agent once it is listening.
type Ready struct {
	Version int    `json:"version"`
	Kernel  string `json:"kernel"`
}

// FileOpen starts a file transfer into the guest.
type FileOpen struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}

// Result reports the outcome of a file transfer.
type Result struct {
	Error string `json:"error,omitempty"`
}

// Exec asks the agent to run a command.
type Exec struct {
	Args []string `json:"args"`
	Env  []string `json:"env,omitempty"` // added to the default environment
	Dir  string   `json:"dir,omitempty"`
}

// Exit reports how a command ended.
type Exit struct {
	Code   int    `json:"code"`             // exit status, or 128+signal
	Signal string `json:"signal,omitempty"` // set if killed by a signal
	Error  string `json:"error,omitempty"`  // set if the command could not start
}

// Frame is one decoded message.
type Frame struct {
	Type    uint8
	Payload []byte
}

// ReadFrame reads one frame, rejecting payloads larger than max.
func ReadFrame(r io.Reader, max int) (Frame, error) {
	var hdr [headerSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > uint32(max) {
		return Frame{}, fmt.Errorf("proto: frame type %#x of %d bytes exceeds limit %d", hdr[0], n, max)
	}
	f := Frame{Type: hdr[0], Payload: make([]byte, n)}
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		return Frame{}, fmt.Errorf("proto: truncated frame: %w", err)
	}
	return f, nil
}

// encode returns the wire form of a frame.
func encode(typ uint8, payload []byte) []byte {
	b := make([]byte, headerSize+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:], uint32(len(payload)))
	copy(b[headerSize:], payload)
	return b
}

// Decode unmarshals a JSON frame payload into v, rejecting unknown fields.
func Decode(f Frame, v any) error {
	if err := json.Unmarshal(f.Payload, v); err != nil {
		return fmt.Errorf("proto: bad payload for frame %#x: %w", f.Type, err)
	}
	return nil
}

// FrameSize is the number of bytes a frame occupies on the wire; credit is
// counted in these units.
func FrameSize(f Frame) int { return headerSize + len(f.Payload) }

// Writer serializes frames from multiple goroutines onto one stream.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter returns a frame writer on w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write sends one raw frame.
func (fw *Writer) Write(typ uint8, payload []byte) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	_, err := fw.w.Write(encode(typ, payload))
	return err
}

// WriteJSON sends one frame with a JSON payload.
func (fw *Writer) WriteJSON(typ uint8, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return fw.Write(typ, b)
}

// Credit sends a credit grant (guest side).
func (fw *Writer) Credit(n int) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(n))
	return fw.Write(TypeCredit, b[:])
}

// ErrClosed is returned by CreditWriter after Close.
var ErrClosed = errors.New("proto: connection closed")

// CreditWriter is the host's flow-controlled frame writer. Each frame
// waits until the guest has granted enough credit.
type CreditWriter struct {
	mu     sync.Mutex
	cond   *sync.Cond
	w      io.Writer
	credit int
	closed bool
}

// NewCreditWriter returns a writer starting with InitialCredit.
func NewCreditWriter(w io.Writer) *CreditWriter {
	c := &CreditWriter{w: w, credit: InitialCredit}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Grant adds credit received from the guest.
func (c *CreditWriter) Grant(n int) {
	c.mu.Lock()
	// Never let a hostile guest overflow the counter.
	c.credit = min(c.credit+n, 1<<30)
	c.mu.Unlock()
	c.cond.Broadcast()
}

// Close wakes any blocked writers with ErrClosed.
func (c *CreditWriter) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cond.Broadcast()
}

// Write sends one frame once credit allows. Payloads larger than
// MaxHostFrame are rejected; callers split data into chunks.
func (c *CreditWriter) Write(typ uint8, payload []byte) error {
	if len(payload) > MaxHostFrame {
		return fmt.Errorf("proto: host frame of %d bytes exceeds %d", len(payload), MaxHostFrame)
	}
	frame := encode(typ, payload)
	c.mu.Lock()
	for c.credit < len(frame) && !c.closed {
		c.cond.Wait()
	}
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.credit -= len(frame)
	// Holding the lock while writing keeps frames whole and in order.
	_, err := c.w.Write(frame)
	c.mu.Unlock()
	return err
}

// WriteJSON sends one JSON frame once credit allows.
func (c *CreditWriter) WriteJSON(typ uint8, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(typ, b)
}

// ParseCredit decodes a TypeCredit payload.
func ParseCredit(f Frame) (int, error) {
	if len(f.Payload) != 4 {
		return 0, errors.New("proto: malformed credit frame")
	}
	return int(binary.BigEndian.Uint32(f.Payload)), nil
}
