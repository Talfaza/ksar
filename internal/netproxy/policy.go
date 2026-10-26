// Package netproxy is the guest's only way onto the network: a userspace
// TCP/IP stack (gVisor's netstack) on the far end of the VM's virtio-net
// card. The guest never gets a route to the host's network. Each TCP
// connection it opens is terminated here, checked against the policy and,
// if allowed, re-made by the proxy as an ordinary host connection. DNS is
// answered here too, so names can be filtered before they resolve.
//
// Everything arriving from the guest is untrusted. The proxy runs as its
// own process, so a bug in it is contained away from the VMM.
package netproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
)

// Modes. "none" means no network card at all and never reaches the proxy.
const (
	ModeNone     = "none"
	ModeAllow    = "allow"    // only hosts on the allow list
	ModeInternet = "internet" // any public address
)

// Policy is the user's network configuration, as passed to the proxy.
type Policy struct {
	Mode  string   `json:"mode"`
	Allow []string `json:"allow,omitempty"`
}

// rule is one allow-list entry: a host name (exact, or *.suffix for any
// subdomain) or an IPv4 address/prefix, optionally limited to a port.
type rule struct {
	host     string // lower case, no trailing dot; "" for address rules
	wildcard bool   // host is a suffix: *.host
	prefix   netip.Prefix
	port     uint16 // 0 = any
}

func (r rule) String() string {
	s := r.host
	if r.wildcard {
		s = "*." + s
	}
	if r.host == "" {
		s = r.prefix.String()
		if r.prefix.IsSingleIP() {
			s = r.prefix.Addr().String()
		}
	}
	if r.port != 0 {
		s += ":" + strconv.Itoa(int(r.port))
	}
	return s
}

func (r rule) matchesName(name string) bool {
	if r.host == "" {
		return false
	}
	if r.wildcard {
		return strings.HasSuffix(name, "."+r.host)
	}
	return name == r.host
}

func (r rule) matchesPort(port uint16) bool { return r.port == 0 || r.port == port }

// ParseRule parses one allow-list entry: pypi.org, *.pythonhosted.org,
// github.com:443, 1.2.3.4, 10.1.0.0/16:8080.
func ParseRule(s string) (rule, error) {
	var r rule
	orig := s
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return r, errors.New("empty allow rule")
	}
	if host, port, ok := strings.Cut(s, ":"); ok {
		p, err := strconv.ParseUint(port, 10, 16)
		if err != nil || p == 0 {
			return r, fmt.Errorf("allow rule %q: bad port %q", orig, port)
		}
		r.port, s = uint16(p), host
	}
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() {
			return r, fmt.Errorf("allow rule %q: not an IPv4 prefix", orig)
		}
		r.prefix = p.Masked()
		return r, nil
	}
	if a, err := netip.ParseAddr(s); err == nil {
		if !a.Is4() {
			return r, fmt.Errorf("allow rule %q: the guest network is IPv4 only", orig)
		}
		r.prefix = netip.PrefixFrom(a, 32)
		return r, nil
	}
	if rest, ok := strings.CutPrefix(s, "*."); ok {
		r.wildcard, s = true, rest
	}
	s = strings.TrimSuffix(s, ".")
	if !validHost(s) {
		return r, fmt.Errorf("allow rule %q: not a host name, address or prefix", orig)
	}
	r.host = s
	return r, nil
}

func validHost(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// blockedPrefixes are never reachable unless an address rule names them:
// they lead to the host itself, the local network or cloud metadata.
var blockedPrefixes = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // "this network"
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // carrier-grade NAT (also Tailscale)
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link local, incl. cloud metadata 169.254.169.254
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.168.0.0/16",  // private
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, incl. broadcast
	} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return ps
}()

// Checker is a compiled policy plus what the proxy's DNS has answered,
// which is how host-name rules apply to the addresses the guest dials.
type Checker struct {
	mode  string
	rules []rule
	local map[netip.Addr]bool // the host's own addresses

	mu    sync.Mutex
	names map[netip.Addr]map[string]bool // address -> names DNS resolved to it
}

// maxNames bounds the address->name table against a guest spamming lookups.
const maxNames = 8192

// Compile checks a policy and prepares it for use.
func Compile(p Policy) (*Checker, error) {
	c := &Checker{mode: p.Mode, names: map[netip.Addr]map[string]bool{}, local: map[netip.Addr]bool{}}
	switch p.Mode {
	case ModeAllow, ModeInternet:
	case ModeNone, "":
		return nil, errors.New("network mode none needs no proxy")
	default:
		return nil, fmt.Errorf("unknown network mode %q (want none, allow or internet)", p.Mode)
	}
	for _, s := range p.Allow {
		r, err := ParseRule(s)
		if err != nil {
			return nil, err
		}
		c.rules = append(c.rules, r)
	}
	if p.Mode == ModeAllow && len(c.rules) == 0 {
		return nil, errors.New("network mode allow needs at least one allow rule")
	}
	// The host's own addresses, including public ones: services listening
	// on all interfaces would otherwise be reachable through them.
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok {
					c.local[ip.Unmap()] = true
				}
			}
		}
	}
	return c, nil
}

// Rules returns the allow list in canonical form.
func (c *Checker) Rules() []string {
	var out []string
	for _, r := range c.rules {
		out = append(out, r.String())
	}
	return out
}

// addressRule reports whether an address rule covers ip:port. Address
// rules are the only way past the blocked ranges.
func (c *Checker) addressRule(ip netip.Addr, port uint16) bool {
	for _, r := range c.rules {
		if r.host == "" && r.prefix.Contains(ip) && r.matchesPort(port) {
			return true
		}
	}
	return false
}

// blocked reports why ip is off limits by default ("" if it is not).
func (c *Checker) blocked(ip netip.Addr) string {
	if !ip.Is4() {
		return "not IPv4"
	}
	if c.local[ip] {
		return "address of this machine"
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return "private or reserved address"
		}
	}
	return ""
}

// AllowName reports whether the guest may resolve name.
func (c *Checker) AllowName(name string) bool {
	if c.mode == ModeInternet {
		return true
	}
	for _, r := range c.rules {
		if r.matchesName(name) {
			return true
		}
	}
	return false
}

// AllowResolved filters a DNS answer: addresses in blocked ranges are
// dropped (a public name pointing at the local network is a DNS rebinding
// attempt) unless an address rule allows them.
func (c *Checker) AllowResolved(ip netip.Addr) bool {
	for _, r := range c.rules {
		if r.host == "" && r.prefix.Contains(ip) {
			return true // allowed on some port
		}
	}
	return c.blocked(ip) == ""
}

// Remember records that DNS resolved name to ip for the guest.
func (c *Checker) Remember(name string, ip netip.Addr) {
	c.mu.Lock()
	defer c.mu.Unlock()
	set := c.names[ip]
	if set == nil {
		if len(c.names) >= maxNames {
			return
		}
		set = map[string]bool{}
		c.names[ip] = set
	}
	if len(set) < 16 {
		set[name] = true
	}
}

// Decision is the verdict on one connection attempt.
type Decision struct {
	Allowed bool
	Host    string // the name the guest looked up for this address, if any
	Reason  string
}

// Connect decides whether the guest may open a connection to ip:port.
// The proxy dials exactly this address, so what is checked is what is
// reached: no second lookup an attacker could race.
func (c *Checker) Connect(ip netip.Addr, port uint16) Decision {
	c.mu.Lock()
	var names []string
	for n := range c.names[ip] {
		names = append(names, n)
	}
	c.mu.Unlock()
	host := ""
	if len(names) > 0 {
		host = names[0]
	}
	if c.addressRule(ip, port) {
		return Decision{true, host, "address on the allow list"}
	}
	if why := c.blocked(ip); why != "" {
		return Decision{false, host, why}
	}
	if c.mode == ModeInternet {
		return Decision{true, host, "internet access"}
	}
	for _, n := range names {
		for _, r := range c.rules {
			if r.matchesName(n) && r.matchesPort(port) {
				return Decision{true, n, "allowed host " + r.String()}
			}
		}
	}
	if host != "" {
		return Decision{false, host, "port not allowed for this host"}
	}
	return Decision{false, "", "address not looked up through an allowed name"}
}

// Encode serializes a policy for the proxy's command line.
func (p Policy) Encode() string {
	b, _ := json.Marshal(p)
	return string(b)
}

// DecodePolicy is the inverse of Encode.
func DecodePolicy(s string) (Policy, error) {
	var p Policy
	err := json.Unmarshal([]byte(s), &p)
	return p, err
}
