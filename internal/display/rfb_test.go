package display

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"ksar/internal/virtio"
)

func testScreen() *Screen {
	w, h := 200, 100
	return &Screen{Width: w, Height: h, Stride: w * 4, FB: make([]byte, w*h*4), Name: "test",
		Keyboard: virtio.NewInput(virtio.KeyboardSpec()), Tablet: virtio.NewInput(virtio.TabletSpec(w, h))}
}

type viewer struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func (v *viewer) read(n int) []byte {
	v.t.Helper()
	b := make([]byte, n)
	v.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(v.r, b); err != nil {
		v.t.Fatalf("read %d: %v", n, err)
	}
	return b
}

// dial connects a viewer and completes the handshake.
func dial(t *testing.T, s *Screen, auth Auth, password string) (*viewer, error) {
	t.Helper()
	a, b := net.Pipe()
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(a, auth) }()
	t.Cleanup(func() { b.Close() })
	v := &viewer{t: t, c: b, r: bufio.NewReader(b)}
	v.read(12)
	b.Write([]byte("RFB 003.008\n"))
	types := v.read(2)
	b.Write(types[1:2])
	if types[1] == 2 {
		ch := v.read(16)
		b.Write(vncEncrypt(password, ch))
	}
	if res := binary.BigEndian.Uint32(v.read(4)); res != 0 {
		v.read(4 + len("wrong password"))
		b.Close()
		return nil, <-errc
	}
	b.Write([]byte{1})
	si := v.read(24)
	v.read(int(binary.BigEndian.Uint32(si[20:])))
	if binary.BigEndian.Uint16(si[0:]) != uint16(s.Width) || binary.BigEndian.Uint16(si[2:]) != uint16(s.Height) {
		t.Fatalf("ServerInit size wrong")
	}
	return v, nil
}

func (v *viewer) update(incremental bool, s *Screen, bpp int) (rects [][4]int, pix [][]byte) {
	req := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if incremental {
		req[1] = 1
	}
	binary.BigEndian.PutUint16(req[6:], uint16(s.Width))
	binary.BigEndian.PutUint16(req[8:], uint16(s.Height))
	v.c.Write(req)
	h := v.read(4)
	for i := 0; i < int(binary.BigEndian.Uint16(h[2:])); i++ {
		rh := v.read(12)
		r := [4]int{int(binary.BigEndian.Uint16(rh[0:])), int(binary.BigEndian.Uint16(rh[2:])),
			int(binary.BigEndian.Uint16(rh[4:])), int(binary.BigEndian.Uint16(rh[6:]))}
		rects = append(rects, r)
		pix = append(pix, v.read(r[2]*r[3]*bpp))
	}
	return rects, pix
}

func TestAuth(t *testing.T) {
	s := testScreen()
	if _, err := dial(t, s, Auth{Password: "secret12"}, "wrong"); err == nil {
		t.Error("wrong password accepted")
	}
	if _, err := dial(t, s, Auth{Password: "secret12"}, "secret12"); err != nil {
		t.Errorf("right password refused: %v", err)
	}
}

func TestUpdates(t *testing.T) {
	s := testScreen()
	v, err := dial(t, s, Auth{}, "")
	if err != nil {
		t.Fatal(err)
	}
	rects, _ := v.update(false, s, 4)
	total := 0
	for _, r := range rects {
		total += r[2] * r[3]
	}
	if total != s.Width*s.Height {
		t.Fatalf("full update covered %d pixels, want %d", total, s.Width*s.Height)
	}
	// Change one pixel: the incremental update is just its tile.
	o := 70*s.Stride + 130*4
	s.FB[o], s.FB[o+1], s.FB[o+2] = 1, 2, 3
	rects, pix := v.update(true, s, 4)
	if len(rects) != 1 || rects[0] != [4]int{128, 64, 64, 36} {
		t.Fatalf("incremental update = %v, want the one tile at 128,64", rects)
	}
	p := pix[0][(70-64)*64*4+(130-128)*4:]
	if p[0] != 1 || p[1] != 2 || p[2] != 3 {
		t.Errorf("pixel = % x", p[:4])
	}
	// RGB565, little endian: red 5 bits at 11.
	v.c.Write([]byte{0, 0, 0, 0, 16, 16, 0, 1, 0, 31, 0, 63, 0, 31, 11, 5, 0, 0, 0, 0})
	s.FB[0], s.FB[1], s.FB[2] = 0, 0, 255 // pure red
	_, pix = v.update(false, s, 2)
	if got := binary.LittleEndian.Uint16(pix[0]); got != 31<<11 {
		t.Errorf("rgb565 red = %#x, want %#x", got, 31<<11)
	}
}

func TestInput(t *testing.T) {
	s := testScreen()
	v, err := dial(t, s, Auth{}, "")
	if err != nil {
		t.Fatal(err)
	}
	v.c.Write([]byte{4, 1, 0, 0, 0, 0, 0, 'A'})              // 'A' down
	v.c.Write([]byte{5, 1, 0, 50, 0, 20})                    // left button at 50,20
	v.c.Write([]byte{5, 1 | 8, 0x10, 0, 0x10, 0})            // wheel up, far outside: clamped
	v.c.Write([]byte{6, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c'}) // clipboard: ignored
	v.c.Write([]byte{3, 1, 0, 0, 0, 0, 0, 1, 0, 1})          // sync: wait for processing
	time.Sleep(100 * time.Millisecond)
	k := s.Keyboard.Pending()
	if len(k) != 2 || k[0] != (virtio.InputEvent{Type: virtio.EvKey, Code: 30, Value: 1}) {
		t.Errorf("keyboard events = %+v", k)
	}
	want := []virtio.InputEvent{
		{Type: virtio.EvAbs, Code: virtio.AbsX, Value: 50}, {Type: virtio.EvAbs, Code: virtio.AbsY, Value: 20},
		{Type: virtio.EvKey, Code: virtio.BtnLeft, Value: 1}, {Type: virtio.EvSyn},
		{Type: virtio.EvAbs, Code: virtio.AbsX, Value: 199}, {Type: virtio.EvAbs, Code: virtio.AbsY, Value: 99},
		{Type: virtio.EvRel, Code: virtio.RelWheel, Value: 1}, {Type: virtio.EvSyn},
	}
	got := s.Tablet.Pending()
	if len(got) != len(want) {
		t.Fatalf("tablet events = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tablet event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestHostileClient(t *testing.T) {
	s := testScreen()
	for name, msg := range map[string][]byte{
		"huge clipboard":   {6, 0, 0, 0, 0x7f, 0xff, 0xff, 0xff},
		"negative length":  {6, 0, 0, 0, 0xff, 0xff, 0xff, 0xff},
		"palette format":   {0, 0, 0, 0, 8, 8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		"unknown message":  {99},
		"shift past width": {0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 40, 8, 0, 0, 0, 0},
	} {
		a, b := net.Pipe()
		errc := make(chan error, 1)
		go func() { errc <- s.Serve(a, Auth{}) }()
		v := &viewer{t: t, c: b, r: bufio.NewReader(b)}
		v.read(12)
		b.Write([]byte("RFB 003.008\n"))
		v.read(2)
		b.Write([]byte{1})
		v.read(4)
		b.Write([]byte{1})
		si := v.read(24)
		v.read(int(binary.BigEndian.Uint32(si[20:])))
		b.Write(msg)
		select {
		case err := <-errc:
			if err == nil {
				t.Errorf("%s: connection ended without an error", name)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s: server kept the connection", name)
		}
		b.Close()
	}
}

func TestKeymap(t *testing.T) {
	for ks, want := range map[uint32]uint16{'a': 30, 'A': 30, '1': 2, '!': 2, '0': 11, ' ': 57, 0xff0d: 28,
		0xffe1: 42, 0xffbe: 59, 0xffc9: 88, '|': 43, 0x12345: 0} {
		if got := KeyCode(ks); got != want {
			t.Errorf("KeyCode(%#x) = %d, want %d", ks, got, want)
		}
	}
}

// FuzzServe feeds arbitrary viewer bytes after a valid handshake. The
// server may end the connection, but must not panic or touch memory
// outside the framebuffer.
func FuzzServe(f *testing.F) {
	f.Add([]byte{3, 0, 0, 0, 0, 0, 0, 200, 0, 100})
	f.Add([]byte{3, 1, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{5, 0xff, 0xff, 0xff, 0xff, 0xff, 4, 1, 0, 0, 0, 0, 0xff, 0x0d})
	f.Add([]byte{0, 0, 0, 0, 16, 16, 1, 1, 0, 31, 0, 63, 0, 31, 11, 5, 0, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 10, 0, 10})
	f.Fuzz(func(t *testing.T, data []byte) {
		s := testScreen()
		a, b := net.Pipe()
		done := make(chan struct{})
		go func() { s.Serve(a, Auth{}); close(done) }()
		go io.Copy(io.Discard, b)
		prefix := append([]byte("RFB 003.008\n"), 1, 1)
		b.SetWriteDeadline(time.Now().Add(time.Second))
		b.Write(append(prefix, data...))
		time.Sleep(5 * time.Millisecond)
		b.Close()
		<-done
	})
}
