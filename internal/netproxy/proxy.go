package netproxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/link/nested"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// The guest network, as the guest sees it.
var (
	GuestAddr   = netip.MustParseAddr("10.0.2.15")
	GatewayAddr = netip.MustParseAddr("10.0.2.2")
	DNSAddr     = netip.MustParseAddr("10.0.2.3")
	Netmask     = "255.255.255.0"
	GuestMAC    = [6]byte{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}
	gatewayMAC  = tcpip.LinkAddress("\x52\x54\x00\x12\x34\x02")
)

const (
	mtu = 1500
	// Limits on what one guest can make the proxy hold open.
	maxConns    = 256
	maxDNS      = 256 // open DNS sessions (one per guest socket)
	dialTimeout = 10 * time.Second
	// Resolvers use a fresh socket per lookup, so a session only needs to
	// outlive the A/AAAA pair and a retry.
	dnsIdle = 3 * time.Second
)

// Event is one logged network action.
type Event struct {
	Kind     string   `json:"kind"` // dns, connect, close, udp
	Name     string   `json:"name,omitempty"`
	Dst      string   `json:"dst,omitempty"`
	Allowed  bool     `json:"allowed"`
	Reason   string   `json:"reason,omitempty"`
	Answers  []string `json:"answers,omitempty"`
	Sent     int64    `json:"sent,omitempty"`     // guest -> world bytes
	Received int64    `json:"received,omitempty"` // world -> guest bytes
	Millis   int64    `json:"ms,omitempty"`
}

// Proxy serves one VM's network.
type Proxy struct {
	check  *Checker
	lookup lookupFunc
	emit   func(Event)
	dial   func(network, addr string) (net.Conn, error)

	conns atomic.Int32
	dns   atomic.Int32

	udpMu   sync.Mutex
	udpSeen map[string]bool // blocked UDP destinations already logged
}

// New creates a proxy. emit receives every network event; it must be
// safe for concurrent use.
func New(c *Checker, emit func(Event)) *Proxy {
	d := &net.Dialer{Timeout: dialTimeout}
	return &Proxy{check: c, lookup: hostLookup, emit: emit, dial: d.Dial, udpSeen: map[string]bool{}}
}

// Serve runs the guest's network over fd, a SOCK_SEQPACKET socket with
// one Ethernet frame per message, until the other end closes.
func (p *Proxy) Serve(fd int) error {
	if err := syscall.SetNonblock(fd, true); err != nil {
		return err
	}
	closed := make(chan tcpip.Error, 1)
	link, err := fdbased.New(&fdbased.Options{
		FDs: []int{fd}, MTU: mtu, EthernetHeader: true, Address: gatewayMAC,
		ClosedFunc: func(e tcpip.Error) { closed <- e },
	})
	if err != nil {
		return fmt.Errorf("network link: %v", err)
	}
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	defer s.Close()
	if e := s.CreateNIC(1, newGuestFilter(link)); e != nil {
		return fmt.Errorf("network card: %v", e)
	}
	// The stack answers for every address (promiscuous) and may send from
	// any (spoofing): it stands in for the whole internet, with each
	// connection checked by the forwarders below.
	s.SetPromiscuousMode(1, true)
	s.SetSpoofing(1, true)
	for _, a := range []netip.Addr{GatewayAddr, DNSAddr} {
		pa := tcpip.ProtocolAddress{
			Protocol:          ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddrFrom4(a.As4()).WithPrefix(),
		}
		pa.AddressWithPrefix.PrefixLen = 24
		if e := s.AddProtocolAddress(1, pa, stack.AddressProperties{}); e != nil {
			return fmt.Errorf("network address %v: %v", a, e)
		}
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})

	tcpFwd := tcp.NewForwarder(s, 0, maxConns, p.handleTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
	udpFwd := udp.NewForwarder(s, p.handleUDP)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	// The link closes when the VM's end of the socket does; that is the
	// normal way for the proxy to stop.
	switch e := (<-closed).(type) {
	case nil, *tcpip.ErrClosedForReceive, *tcpip.ErrConnectionReset, *tcpip.ErrConnectionAborted:
		return nil
	default:
		return fmt.Errorf("network link closed: %v", e)
	}
}

func addrOf(a tcpip.Address) netip.Addr {
	if a.Len() != 4 {
		return netip.Addr{}
	}
	return netip.AddrFrom4(a.As4())
}

func (p *Proxy) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst := addrOf(id.LocalAddress)
	target := netip.AddrPortFrom(dst, id.LocalPort).String()
	ev := Event{Kind: "connect", Dst: target}
	var d Decision
	if dst == GatewayAddr || dst == DNSAddr {
		d = Decision{Reason: "nothing listens on the gateway"}
	} else {
		d = p.check.Connect(dst, id.LocalPort)
	}
	ev.Name, ev.Reason, ev.Allowed = d.Host, d.Reason, d.Allowed
	if !d.Allowed {
		p.emit(ev)
		r.Complete(true)
		return
	}
	if p.conns.Add(1) > maxConns {
		p.conns.Add(-1)
		ev.Allowed, ev.Reason = false, "too many open connections"
		p.emit(ev)
		r.Complete(true)
		return
	}
	defer p.conns.Add(-1)

	start := time.Now()
	// Dial first, so a refused or unreachable host reaches the guest as a
	// reset rather than a connection that dies right after opening.
	out, err := p.dial("tcp4", target)
	if err != nil {
		ev.Allowed, ev.Reason = false, "connect failed: "+shortErr(err)
		p.emit(ev)
		r.Complete(true)
		return
	}
	defer out.Close()
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		r.Complete(true)
		return
	}
	r.Complete(false)
	p.emit(ev)
	in := gonet.NewTCPConn(&wq, ep)
	defer in.Close()

	var sent, received int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sent, _ = io.Copy(out, in)
		if c, ok := out.(*net.TCPConn); ok {
			c.CloseWrite()
		}
	}()
	received, _ = io.Copy(in, out)
	in.CloseWrite()
	wg.Wait()
	p.emit(Event{Kind: "close", Dst: target, Name: d.Host, Allowed: true,
		Sent: sent, Received: received, Millis: time.Since(start).Milliseconds()})
}

func (p *Proxy) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst := addrOf(id.LocalAddress)
	if dst == DNSAddr && id.LocalPort == 53 {
		if p.dns.Add(1) > maxDNS {
			p.dns.Add(-1)
			return true // dropped; the guest's resolver retries
		}
		var wq waiter.Queue
		ep, err := r.CreateEndpoint(&wq)
		if err != nil {
			p.dns.Add(-1)
			return true
		}
		go func() {
			defer p.dns.Add(-1)
			p.serveDNS(gonet.NewUDPConn(&wq, ep))
		}()
		return true
	}
	// Other UDP is not proxied. Log each destination once, then let the
	// stack answer "port unreachable" so the guest fails fast.
	target := netip.AddrPortFrom(dst, id.LocalPort).String()
	p.udpMu.Lock()
	first := !p.udpSeen[target] && len(p.udpSeen) < 1024
	if first {
		p.udpSeen[target] = true
	}
	p.udpMu.Unlock()
	if first {
		p.emit(Event{Kind: "udp", Dst: target, Reason: "only DNS (to " + DNSAddr.String() + ") is supported over UDP"})
	}
	return false
}

func (p *Proxy) serveDNS(c *gonet.UDPConn) {
	defer c.Close()
	buf := make([]byte, 1500)
	for {
		c.SetReadDeadline(time.Now().Add(dnsIdle))
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		if reply := p.answerDNS(buf[:n]); reply != nil {
			c.Write(reply)
		}
	}
}

func shortErr(err error) string {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err.Error()
	}
	return err.Error()
}

// guestFilter sits between the link and the stack and drops what the stack
// would otherwise answer on the internet's behalf: in promiscuous mode it
// replies to pings for any address, which would make every host look up.
// Pings to the gateway and the DNS server still work.
type guestFilter struct {
	nested.Endpoint
}

func newGuestFilter(child stack.LinkEndpoint) *guestFilter {
	f := &guestFilter{}
	f.Endpoint.Init(child, f)
	return f
}

func (f *guestFilter) DeliverNetworkPacket(protocol tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	switch protocol {
	case arp.ProtocolNumber:
	case ipv4.ProtocolNumber:
		h, ok := pkt.Data().PullUp(header.IPv4MinimumSize)
		if !ok {
			return
		}
		ip := header.IPv4(h)
		if ip.TransportProtocol() == header.ICMPv4ProtocolNumber {
			if d := addrOf(ip.DestinationAddress()); d != GatewayAddr && d != DNSAddr {
				return
			}
		}
	default:
		return // IPv4 only
	}
	f.Endpoint.DeliverNetworkPacket(protocol, pkt)
}

// FormatEvent renders an event for a terminal.
func FormatEvent(e Event) string {
	what := e.Dst
	if e.Name != "" && e.Dst != "" {
		what = e.Name + " (" + e.Dst + ")"
	} else if e.Name != "" {
		what = e.Name
	}
	switch e.Kind {
	case "dns":
		if !e.Allowed {
			return "blocked lookup " + what + ": " + e.Reason
		}
		if len(e.Answers) == 0 {
			return "lookup " + what + ": " + e.Reason
		}
		return fmt.Sprintf("lookup %s -> %v", what, e.Answers)
	case "connect":
		if !e.Allowed {
			return "blocked connection to " + what + ": " + e.Reason
		}
		return "connect " + what
	case "close":
		return "closed " + what + ": sent " + strconv.FormatInt(e.Sent, 10) +
			" B, received " + strconv.FormatInt(e.Received, 10) + " B"
	case "udp":
		return "blocked UDP to " + what + ": " + e.Reason
	}
	return e.Kind + " " + what
}
