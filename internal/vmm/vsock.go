package vmm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	afVsock        = 40
	vmaddrCIDAny   = 0xffffffff
	vmaddrPortAny  = 0xffffffff
	sockaddrVMSize = 16
)

// sockaddrVM is struct sockaddr_vm.
type sockaddrVM [sockaddrVMSize]byte

func newSockaddrVM(cid, port uint32) sockaddrVM {
	var sa sockaddrVM
	binary.LittleEndian.PutUint16(sa[0:], afVsock)
	binary.LittleEndian.PutUint32(sa[4:], port)
	binary.LittleEndian.PutUint32(sa[8:], cid)
	return sa
}

func (sa *sockaddrVM) port() uint32 { return binary.LittleEndian.Uint32(sa[4:]) }
func (sa *sockaddrVM) cid() uint32  { return binary.LittleEndian.Uint32(sa[8:]) }

// agentListener waits for the guest agent to connect over vsock.
//
// The host listens and the guest dials out, not the other way round: any
// local process can dial a guest's vsock port, but the source CID of an
// incoming connection is set by the kernel and can't be forged. Only a
// connection from our own VM's CID is accepted.
type agentListener struct {
	fd       int
	port     uint32
	guestCID uint32
}

func listenAgent(guestCID uint32) (*agentListener, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	sa := newSockaddrVM(vmaddrCIDAny, vmaddrPortAny) // kernel picks a free port
	if _, _, e := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), sockaddrVMSize); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("vsock bind: %w", e)
	}
	if err := syscall.Listen(fd, 4); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	var bound sockaddrVM
	n := uint32(sockaddrVMSize)
	if _, _, e := syscall.Syscall(syscall.SYS_GETSOCKNAME, uintptr(fd),
		uintptr(unsafe.Pointer(&bound)), uintptr(unsafe.Pointer(&n))); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("vsock getsockname: %w", e)
	}
	return &agentListener{fd: fd, port: bound.port(), guestCID: guestCID}, nil
}

// accept returns the agent's connection. It gives up when done is closed.
// Connections from any other CID are closed and ignored.
func (l *agentListener) accept(done <-chan struct{}) (*os.File, error) {
	for {
		select {
		case <-done:
			return nil, errors.New("VM stopped before the agent connected")
		default:
		}
		var peer sockaddrVM
		n := uint32(sockaddrVMSize)
		nfd, _, e := syscall.Syscall6(syscall.SYS_ACCEPT4, uintptr(l.fd),
			uintptr(unsafe.Pointer(&peer)), uintptr(unsafe.Pointer(&n)),
			syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0, 0)
		switch {
		case e == syscall.EAGAIN || e == syscall.EINTR:
			waitReadable(l.fd, 20*time.Millisecond)
			continue
		case e != 0:
			return nil, fmt.Errorf("vsock accept: %w", e)
		}
		if peer.cid() != l.guestCID {
			syscall.Close(int(nfd))
			continue
		}
		// Non-blocking, so os.File reads go through the runtime poller
		// and Close wakes blocked readers.
		return os.NewFile(nfd, "vsock-agent"), nil
	}
}

func (l *agentListener) close() error { return syscall.Close(l.fd) }

// waitReadable blocks until fd is readable or the timeout passes.
func waitReadable(fd int, timeout time.Duration) {
	var set syscall.FdSet
	set.Bits[fd/64] |= 1 << (uint(fd) % 64)
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	syscall.Select(fd+1, &set, nil, nil, &tv)
}
