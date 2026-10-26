// Package agent is the host side of the Ksar guest agent protocol.
package agent

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"ksar/internal/agent/proto"
)

// Client talks to ksar-agent over a byte stream (the VM's agent channel).
// Everything the guest sends is untrusted and validated.
type Client struct {
	cw     *proto.CreditWriter
	frames chan proto.Frame

	mu      sync.Mutex
	readErr error
}

// NewClient starts reading frames from rw.
func NewClient(rw io.ReadWriter) *Client {
	c := &Client{
		cw:     proto.NewCreditWriter(rw),
		frames: make(chan proto.Frame, 64),
	}
	go c.readLoop(rw)
	return c
}

func (c *Client) readLoop(r io.Reader) {
	defer close(c.frames)
	defer c.cw.Close()
	for {
		f, err := proto.ReadFrame(r, proto.MaxGuestFrame)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("agent disconnected (the VM stopped)")
			}
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			return
		}
		if f.Type == proto.TypeCredit {
			n, err := proto.ParseCredit(f)
			if err != nil {
				c.mu.Lock()
				c.readErr = err
				c.mu.Unlock()
				return
			}
			c.cw.Grant(n)
			continue
		}
		c.frames <- f
	}
}

// next returns the next non-credit frame.
func (c *Client) next() (proto.Frame, error) {
	f, ok := <-c.frames
	if !ok {
		c.mu.Lock()
		defer c.mu.Unlock()
		return proto.Frame{}, c.readErr
	}
	return f, nil
}

// expect returns the next frame, failing if it is not of type typ.
func (c *Client) expect(typ uint8) (proto.Frame, error) {
	f, err := c.next()
	if err != nil {
		return f, err
	}
	if f.Type != typ {
		return f, fmt.Errorf("agent protocol error: got frame %#x, want %#x", f.Type, typ)
	}
	return f, nil
}

// WaitReady blocks until the agent announces itself.
func (c *Client) WaitReady() (proto.Ready, error) {
	var r proto.Ready
	f, err := c.expect(proto.TypeReady)
	if err != nil {
		return r, err
	}
	if err := proto.Decode(f, &r); err != nil {
		return r, err
	}
	if r.Version != proto.Version {
		return r, fmt.Errorf("agent speaks protocol v%d, ksar speaks v%d", r.Version, proto.Version)
	}
	return r, nil
}

// Hello sends the run's id and entropy for the guest RNG. It must come
// before anything else, so no command runs with a predictable RNG.
func (c *Client) Hello(id string, entropy []byte) error {
	return c.cw.WriteJSON(proto.TypeHello, proto.Hello{ID: id, Entropy: entropy})
}

// PutFile writes data from r to path inside the guest.
func (c *Client) PutFile(path string, mode uint32, r io.Reader) error {
	if err := c.cw.WriteJSON(proto.TypeFileOpen, proto.FileOpen{Path: path, Mode: mode}); err != nil {
		return err
	}
	buf := make([]byte, proto.MaxHostFrame)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := c.cw.Write(proto.TypeFileData, buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if err := c.cw.Write(proto.TypeFileClose, nil); err != nil {
		return err
	}
	f, err := c.expect(proto.TypeResult)
	if err != nil {
		return err
	}
	var res proto.Result
	if err := proto.Decode(f, &res); err != nil {
		return err
	}
	if res.Error != "" {
		return fmt.Errorf("copy to %s: %s", path, res.Error)
	}
	return nil
}

// PutDir streams the tar archive r into directory dir in the guest and
// returns how many entries were written.
func (c *Client) PutDir(dir string, r io.Reader) (int, error) {
	if err := c.cw.WriteJSON(proto.TypeTarOpen, proto.TarOpen{Dir: dir}); err != nil {
		return 0, err
	}
	buf := make([]byte, proto.MaxHostFrame)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := c.cw.Write(proto.TypeTarData, buf[:n]); werr != nil {
				return 0, werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if err := c.cw.Write(proto.TypeTarClose, nil); err != nil {
		return 0, err
	}
	f, err := c.expect(proto.TypeResult)
	if err != nil {
		return 0, err
	}
	var res proto.Result
	if err := proto.Decode(f, &res); err != nil {
		return 0, err
	}
	if res.Error != "" {
		return res.Files, fmt.Errorf("copy to %s: %s", dir, res.Error)
	}
	return res.Files, nil
}

// Run executes a command in the guest, streaming its output to stdout and
// stderr. If stdin is nil the command gets an empty stdin.
func (c *Client) Run(e proto.Exec, stdin io.Reader, stdout, stderr io.Writer) (proto.Exit, error) {
	var exit proto.Exit
	if err := c.cw.WriteJSON(proto.TypeExec, e); err != nil {
		return exit, err
	}
	go c.sendStdin(stdin)

	for {
		f, err := c.next()
		if err != nil {
			return exit, err
		}
		switch f.Type {
		case proto.TypeStdout:
			stdout.Write(f.Payload)
		case proto.TypeStderr:
			stderr.Write(f.Payload)
		case proto.TypeExit:
			if err := proto.Decode(f, &exit); err != nil {
				return exit, err
			}
			return exit, nil
		default:
			return exit, fmt.Errorf("agent protocol error: unexpected frame %#x during run", f.Type)
		}
	}
}

func (c *Client) sendStdin(stdin io.Reader) {
	if stdin != nil {
		buf := make([]byte, proto.MaxHostFrame)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if c.cw.Write(proto.TypeStdin, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				break
			}
		}
	}
	c.cw.Write(proto.TypeStdinClose, nil)
}

// ErrExportTooLarge means the guest's changes exceeded the export limit.
var ErrExportTooLarge = errors.New("the guest's changes exceed the size limit")

// Export streams the guest's filesystem changes (see proto.Export) as a
// tar into w, writing at most max bytes. The stream comes from the guest
// and is untrusted: callers treat it like any image tar.
func (c *Client) Export(w io.Writer, exclude []string, max int64) (files int, n int64, err error) {
	if err := c.cw.WriteJSON(proto.TypeExport, proto.Export{Exclude: exclude}); err != nil {
		return 0, 0, err
	}
	var werr error
	for {
		f, err := c.next()
		if err != nil {
			return 0, n, err
		}
		switch f.Type {
		case proto.TypeExportData:
			if werr != nil {
				continue // keep draining so the protocol stays in step
			}
			if n+int64(len(f.Payload)) > max {
				werr = ErrExportTooLarge
				continue
			}
			var m int
			m, werr = w.Write(f.Payload)
			n += int64(m)
		case proto.TypeResult:
			var res proto.Result
			if err := proto.Decode(f, &res); err != nil {
				return 0, n, err
			}
			if werr != nil {
				return res.Files, n, werr
			}
			if res.Error != "" {
				return res.Files, n, fmt.Errorf("export: %s", res.Error)
			}
			return res.Files, n, nil
		default:
			return 0, n, fmt.Errorf("agent protocol error: unexpected frame %#x during export", f.Type)
		}
	}
}
