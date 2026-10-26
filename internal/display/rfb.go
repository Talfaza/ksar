// Package display serves the guest's screen over VNC (RFB 3.8) and turns
// the viewer's keyboard and pointer into virtio-input events.
//
// The screen is a plain linear framebuffer in guest memory that the host
// only reads; there is no guest-side display protocol to parse. Viewers
// are untrusted too (any local VNC client), so every client message is
// size-checked and clipboard data from the viewer is discarded.
package display

import (
	"bufio"
	"bytes"
	"crypto/des"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"ksar/internal/virtio"
)

// Screen is a guest framebuffer plus its input devices.
type Screen struct {
	Width, Height, Stride int
	FB                    []byte // host view of guest memory: B, G, R, x per pixel
	Keyboard, Tablet      *virtio.Input
	Name                  string

	inputMu sync.Mutex // serializes event batches from several viewers
}

const (
	tile          = 64
	frameInterval = 33 * time.Millisecond
	maxCutText    = 1 << 20
)

// PixelFormat is an RFB pixel format.
type PixelFormat struct {
	BPP, Depth                      uint8
	BigEndian, TrueColor            bool
	RedMax, GreenMax, BlueMax       uint16
	RedShift, GreenShift, BlueShift uint8
}

// nativeFormat matches the framebuffer's memory layout, so updates are
// plain copies.
var nativeFormat = PixelFormat{BPP: 32, Depth: 24, TrueColor: true,
	RedMax: 255, GreenMax: 255, BlueMax: 255, RedShift: 16, GreenShift: 8, BlueShift: 0}

func (p PixelFormat) marshal() []byte {
	b := make([]byte, 16)
	b[0], b[1] = p.BPP, p.Depth
	if p.BigEndian {
		b[2] = 1
	}
	if p.TrueColor {
		b[3] = 1
	}
	binary.BigEndian.PutUint16(b[4:], p.RedMax)
	binary.BigEndian.PutUint16(b[6:], p.GreenMax)
	binary.BigEndian.PutUint16(b[8:], p.BlueMax)
	b[10], b[11], b[12] = p.RedShift, p.GreenShift, p.BlueShift
	return b
}

func parsePixelFormat(b []byte) PixelFormat {
	return PixelFormat{BPP: b[0], Depth: b[1], BigEndian: b[2] != 0, TrueColor: b[3] != 0,
		RedMax: binary.BigEndian.Uint16(b[4:]), GreenMax: binary.BigEndian.Uint16(b[6:]),
		BlueMax: binary.BigEndian.Uint16(b[8:]), RedShift: b[10], GreenShift: b[11], BlueShift: b[12]}
}

func (p PixelFormat) supported() bool {
	return p.TrueColor && (p.BPP == 8 || p.BPP == 16 || p.BPP == 32) &&
		p.RedMax > 0 && p.GreenMax > 0 && p.BlueMax > 0 &&
		p.RedShift < p.BPP && p.GreenShift < p.BPP && p.BlueShift < p.BPP
}

// Auth selects how viewers authenticate.
type Auth struct {
	// Password, if not empty, requires VNC authentication (only 8 bytes
	// count, as in every VNC server). Empty means no authentication, for
	// a Unix socket whose directory already restricts who can connect.
	Password string
}

// NewPassword returns a random 8-character VNC password.
func NewPassword() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// Serve runs the RFB protocol on one viewer connection until it closes.
func (s *Screen) Serve(conn io.ReadWriteCloser, auth Auth) error {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriterSize(conn, 64<<10)

	if _, err := w.WriteString("RFB 003.008\n"); err != nil {
		return err
	}
	w.Flush()
	ver := make([]byte, 12)
	if _, err := io.ReadFull(r, ver); err != nil {
		return err
	}
	if !bytes.HasPrefix(ver, []byte("RFB 003.")) || ver[11] != '\n' {
		return errors.New("not an RFB client")
	}
	minor := string(ver[8:11])
	if minor < "007" {
		return fmt.Errorf("RFB 3.%s is too old (need 3.7 or 3.8)", minor)
	}
	if err := s.authenticate(r, w, auth, minor >= "008"); err != nil {
		return err
	}
	if _, err := r.ReadByte(); err != nil { // ClientInit: shared flag (always shared)
		return err
	}
	var init [4]byte
	binary.BigEndian.PutUint16(init[0:], uint16(s.Width))
	binary.BigEndian.PutUint16(init[2:], uint16(s.Height))
	w.Write(init[:])
	w.Write(nativeFormat.marshal())
	var nl [4]byte
	binary.BigEndian.PutUint32(nl[:], uint32(len(s.Name)))
	w.Write(nl[:])
	w.WriteString(s.Name)
	if err := w.Flush(); err != nil {
		return err
	}

	c := &client{s: s, w: w, pf: nativeFormat, shadow: make([]byte, len(s.FB)),
		requests: make(chan updateRequest, 1), done: make(chan struct{})}
	go c.updates()
	defer close(c.done)
	return c.readMessages(r)
}

func (s *Screen) authenticate(r *bufio.Reader, w *bufio.Writer, auth Auth, v38 bool) error {
	secType := byte(1) // None
	if auth.Password != "" {
		secType = 2 // VNC authentication
	}
	w.Write([]byte{1, secType})
	w.Flush()
	choice, err := r.ReadByte()
	if err != nil {
		return err
	}
	if choice != secType {
		return errors.New("viewer chose an unsupported security type")
	}
	ok := true
	if secType == 2 {
		challenge := make([]byte, 16)
		rand.Read(challenge)
		w.Write(challenge)
		w.Flush()
		resp := make([]byte, 16)
		if _, err := io.ReadFull(r, resp); err != nil {
			return err
		}
		ok = subtle.ConstantTimeCompare(resp, vncEncrypt(auth.Password, challenge)) == 1
	}
	if ok {
		if secType == 2 || v38 { // 3.7 sends no result for None
			w.Write([]byte{0, 0, 0, 0})
		}
		return w.Flush()
	}
	time.Sleep(time.Second) // slow down guessing
	w.Write([]byte{0, 0, 0, 1})
	if v38 {
		msg := "wrong password"
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(msg)))
		w.Write(n[:])
		w.WriteString(msg)
	}
	w.Flush()
	return errors.New("viewer gave the wrong password")
}

// vncEncrypt is VNC authentication's DES: the key is the password (8
// bytes, zero padded) with each byte's bits reversed.
func vncEncrypt(password string, challenge []byte) []byte {
	key := make([]byte, 8)
	copy(key, password)
	for i, b := range key {
		var r byte
		for j := 0; j < 8; j++ {
			r |= (b >> j & 1) << (7 - j)
		}
		key[i] = r
	}
	block, _ := des.NewCipher(key)
	out := make([]byte, 16)
	block.Encrypt(out[:8], challenge[:8])
	block.Encrypt(out[8:], challenge[8:])
	return out
}

type updateRequest struct {
	incremental bool
	x, y, w, h  int
}

type client struct {
	s      *Screen
	mu     sync.Mutex // guards w, pf and resend
	w      *bufio.Writer
	pf     PixelFormat
	resend bool // the format changed: the next update covers everything

	shadow   []byte // what this viewer has been sent
	requests chan updateRequest
	done     chan struct{}

	buttons uint8
	px, py  int
}

func (c *client) readMessages(r *bufio.Reader) error {
	for {
		t, err := r.ReadByte()
		if err != nil {
			return err
		}
		switch t {
		case 0: // SetPixelFormat
			b := make([]byte, 19)
			if _, err := io.ReadFull(r, b); err != nil {
				return err
			}
			pf := parsePixelFormat(b[3:])
			if !pf.supported() {
				return errors.New("viewer asked for an unsupported pixel format")
			}
			c.mu.Lock()
			c.pf, c.resend = pf, true
			c.mu.Unlock()
		case 2: // SetEncodings: Raw is always used
			b := make([]byte, 3)
			if _, err := io.ReadFull(r, b); err != nil {
				return err
			}
			if _, err := io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint16(b[1:]))*4); err != nil {
				return err
			}
		case 3: // FramebufferUpdateRequest
			b := make([]byte, 9)
			if _, err := io.ReadFull(r, b); err != nil {
				return err
			}
			req := updateRequest{incremental: b[0] != 0,
				x: int(binary.BigEndian.Uint16(b[1:])), y: int(binary.BigEndian.Uint16(b[3:])),
				w: int(binary.BigEndian.Uint16(b[5:])), h: int(binary.BigEndian.Uint16(b[7:]))}
			select { // keep only the latest request
			case <-c.requests:
			default:
			}
			c.requests <- req
		case 4: // KeyEvent
			b := make([]byte, 7)
			if _, err := io.ReadFull(r, b); err != nil {
				return err
			}
			c.key(b[0] != 0, binary.BigEndian.Uint32(b[3:]))
		case 5: // PointerEvent
			b := make([]byte, 5)
			if _, err := io.ReadFull(r, b); err != nil {
				return err
			}
			c.pointer(b[0], int(binary.BigEndian.Uint16(b[1:])), int(binary.BigEndian.Uint16(b[3:])))
		case 6: // ClientCutText: the viewer's clipboard never reaches the guest
			b := make([]byte, 7)
			if _, err := io.ReadFull(r, b); err != nil {
				return err
			}
			n := int32(binary.BigEndian.Uint32(b[3:]))
			if n < 0 || n > maxCutText {
				return errors.New("clipboard text too large")
			}
			if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown RFB message %d", t)
		}
	}
}

func (c *client) key(down bool, keysym uint32) {
	code := KeyCode(keysym)
	if code == 0 || c.s.Keyboard == nil {
		return
	}
	v := int32(0)
	if down {
		v = 1
	}
	c.s.inputMu.Lock()
	c.s.Keyboard.Send(virtio.InputEvent{Type: virtio.EvKey, Code: code, Value: v},
		virtio.InputEvent{Type: virtio.EvSyn, Code: virtio.SynReport})
	c.s.inputMu.Unlock()
}

func (c *client) pointer(mask uint8, x, y int) {
	if c.s.Tablet == nil {
		return
	}
	x, y = min(x, c.s.Width-1), min(y, c.s.Height-1)
	var evs []virtio.InputEvent
	if x != c.px || y != c.py {
		evs = append(evs, virtio.InputEvent{Type: virtio.EvAbs, Code: virtio.AbsX, Value: int32(x)},
			virtio.InputEvent{Type: virtio.EvAbs, Code: virtio.AbsY, Value: int32(y)})
		c.px, c.py = x, y
	}
	changed := mask ^ c.buttons
	for bit, code := range map[uint8]uint16{0: virtio.BtnLeft, 1: virtio.BtnMiddle, 2: virtio.BtnRight} {
		if changed&(1<<bit) != 0 {
			evs = append(evs, virtio.InputEvent{Type: virtio.EvKey, Code: code, Value: int32(mask >> bit & 1)})
		}
	}
	// Wheel "buttons" 4-7 scroll once per press.
	pressed := changed & mask
	for bit, e := range map[uint8]virtio.InputEvent{
		3: {Type: virtio.EvRel, Code: virtio.RelWheel, Value: 1}, 4: {Type: virtio.EvRel, Code: virtio.RelWheel, Value: -1},
		5: {Type: virtio.EvRel, Code: virtio.RelHWheel, Value: -1}, 6: {Type: virtio.EvRel, Code: virtio.RelHWheel, Value: 1},
	} {
		if pressed&(1<<bit) != 0 {
			evs = append(evs, e)
		}
	}
	c.buttons = mask
	if len(evs) == 0 {
		return
	}
	evs = append(evs, virtio.InputEvent{Type: virtio.EvSyn, Code: virtio.SynReport})
	c.s.inputMu.Lock()
	c.s.Tablet.Send(evs...)
	c.s.inputMu.Unlock()
}

// updates answers update requests: a full request immediately, an
// incremental one as soon as part of the screen changed.
func (c *client) updates() {
	t := time.NewTicker(frameInterval)
	defer t.Stop()
	for {
		var req updateRequest
		select {
		case <-c.done:
			return
		case req = <-c.requests:
		}
		x0, y0 := min(req.x, c.s.Width), min(req.y, c.s.Height)
		x1, y1 := min(req.x+req.w, c.s.Width), min(req.y+req.h, c.s.Height)
		for {
			c.mu.Lock()
			all := !req.incremental || c.resend
			c.resend = false
			c.mu.Unlock()
			rects := c.dirty(x0, y0, x1, y1, all)
			if len(rects) > 0 {
				if c.send(rects) != nil {
					return
				}
				break
			}
			select {
			case <-c.done:
				return
			case nr := <-c.requests: // a newer request replaces this one
				req = nr
				x0, y0 = min(req.x, c.s.Width), min(req.y, c.s.Height)
				x1, y1 = min(req.x+req.w, c.s.Width), min(req.y+req.h, c.s.Height)
			case <-t.C:
			}
		}
	}
}

type rect struct{ x, y, w, h int }

// dirty compares the region with what this viewer last got, tile by tile,
// copies changed tiles into the shadow, and returns them as rectangles
// (adjacent tiles in a row merged).
func (c *client) dirty(x0, y0, x1, y1 int, all bool) []rect {
	var out []rect
	s := c.s
	for ty := y0; ty < y1; ty += tile {
		th := min(tile, y1-ty)
		run := -1
		for tx := x0; tx < x1; tx += tile {
			tw := min(tile, x1-tx)
			changed := all
			for y := ty; y < ty+th && !changed; y++ {
				o := y*s.Stride + tx*4
				changed = !bytes.Equal(s.FB[o:o+tw*4], c.shadow[o:o+tw*4])
			}
			if !changed {
				run = -1
				continue
			}
			for y := ty; y < ty+th; y++ {
				o := y*s.Stride + tx*4
				copy(c.shadow[o:o+tw*4], s.FB[o:o+tw*4])
			}
			if run >= 0 && out[run].x+out[run].w == tx {
				out[run].w += tw
			} else {
				out = append(out, rect{tx, ty, tw, th})
				run = len(out) - 1
			}
		}
	}
	return out
}

// send writes one FramebufferUpdate from the shadow (a consistent copy).
func (c *client) send(rects []rect) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.w
	hdr := []byte{0, 0, 0, 0}
	binary.BigEndian.PutUint16(hdr[2:], uint16(len(rects)))
	w.Write(hdr)
	bpp := int(c.pf.BPP) / 8
	line := make([]byte, c.s.Width*bpp)
	for _, r := range rects {
		var rh [12]byte
		binary.BigEndian.PutUint16(rh[0:], uint16(r.x))
		binary.BigEndian.PutUint16(rh[2:], uint16(r.y))
		binary.BigEndian.PutUint16(rh[4:], uint16(r.w))
		binary.BigEndian.PutUint16(rh[6:], uint16(r.h))
		w.Write(rh[:]) // encoding 0: Raw
		for y := r.y; y < r.y+r.h; y++ {
			src := c.shadow[y*c.s.Stride+r.x*4 : y*c.s.Stride+(r.x+r.w)*4]
			if c.pf == nativeFormat {
				w.Write(src)
				continue
			}
			w.Write(convert(line[:r.w*bpp], src, c.pf))
		}
	}
	return w.Flush()
}

// convert re-encodes native pixels (B, G, R, x) into pf.
func convert(dst, src []byte, pf PixelFormat) []byte {
	bpp := int(pf.BPP) / 8
	for i := 0; i*4 < len(src); i++ {
		b, g, r := uint32(src[i*4]), uint32(src[i*4+1]), uint32(src[i*4+2])
		v := (r*uint32(pf.RedMax)/255)<<pf.RedShift | (g*uint32(pf.GreenMax)/255)<<pf.GreenShift |
			(b*uint32(pf.BlueMax)/255)<<pf.BlueShift
		d := dst[i*bpp : i*bpp+bpp]
		switch {
		case bpp == 1:
			d[0] = byte(v)
		case bpp == 2 && pf.BigEndian:
			binary.BigEndian.PutUint16(d, uint16(v))
		case bpp == 2:
			binary.LittleEndian.PutUint16(d, uint16(v))
		case pf.BigEndian:
			binary.BigEndian.PutUint32(d, v)
		default:
			binary.LittleEndian.PutUint32(d, v)
		}
	}
	return dst
}
