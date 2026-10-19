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
