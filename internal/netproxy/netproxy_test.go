package netproxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func mustCompile(t *testing.T, p Policy) *Checker {
	t.Helper()
	c, err := Compile(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseRule(t *testing.T) {
	good := map[string]string{
		"PyPI.org":            "pypi.org",
		"*.pythonhosted.org":  "*.pythonhosted.org",
		"github.com:443":      "github.com:443",
		"1.2.3.4":             "1.2.3.4",
		"10.1.2.3/16:8080":    "10.1.0.0/16:8080",
		"registry.npmjs.org.": "registry.npmjs.org",
	}
	for in, want := range good {
		r, err := ParseRule(in)
		if err != nil {
			t.Errorf("ParseRule(%q): %v", in, err)
			continue
		}
		if r.String() != want {
			t.Errorf("ParseRule(%q) = %q, want %q", in, r, want)
		}
	}
	for _, in := range []string{"", "a..b", "-a.com", "x.com:0", "x.com:99999", "::1", "2001:db8::/32",
		"exa mple.com", "*.", "a.com:http", "fe80::1"} {
		if _, err := ParseRule(in); err == nil {
			t.Errorf("ParseRule(%q) accepted", in)
		}
	}
}

func TestCompile(t *testing.T) {
	if _, err := Compile(Policy{Mode: ModeAllow}); err == nil {
		t.Error("allow mode with no rules accepted")
	}
	if _, err := Compile(Policy{Mode: "everything"}); err == nil {
		t.Error("unknown mode accepted")
	}
	if _, err := Compile(Policy{Mode: ModeNone}); err == nil {
		t.Error("mode none compiled")
	}
}

func TestBlockedRanges(t *testing.T) {
	c := mustCompile(t, Policy{Mode: ModeInternet})
	for _, s := range []string{"127.0.0.1", "10.0.2.2", "192.168.1.1", "172.16.0.1", "169.254.169.254",
		"100.100.100.100", "0.0.0.0", "224.0.0.1", "255.255.255.255"} {
		if d := c.Connect(netip.MustParseAddr(s), 80); d.Allowed {
			t.Errorf("internet mode allowed %s", s)
		}
		if c.AllowResolved(netip.MustParseAddr(s)) {
			t.Errorf("DNS answer %s not filtered", s)
		}
	}
	if d := c.Connect(netip.MustParseAddr("93.184.215.14"), 443); !d.Allowed {
		t.Errorf("internet mode blocked a public address: %s", d.Reason)
	}
	// An explicit address rule is the only way into a private range.
	c = mustCompile(t, Policy{Mode: ModeInternet, Allow: []string{"192.168.1.10:8080"}})
	if !c.Connect(netip.MustParseAddr("192.168.1.10"), 8080).Allowed {
		t.Error("address rule did not open a private address")
	}
	if c.Connect(netip.MustParseAddr("192.168.1.10"), 22).Allowed {
		t.Error("address rule opened the wrong port")
	}
}

func TestAllowMode(t *testing.T) {
	c := mustCompile(t, Policy{Mode: ModeAllow, Allow: []string{"pypi.org", "*.pythonhosted.org", "github.com:443"}})
	for name, want := range map[string]bool{
		"pypi.org": true, "files.pythonhosted.org": true, "pythonhosted.org": false,
		"github.com": true, "evil.com": false, "pypi.org.evil.com": false, "xpypi.org": false,
	} {
		if got := c.AllowName(name); got != want {
			t.Errorf("AllowName(%q) = %v, want %v", name, got, want)
		}
	}
	pypi := netip.MustParseAddr("151.101.0.223")
	gh := netip.MustParseAddr("140.82.112.3")
	// Before a lookup, an address is unknown and refused.
	if c.Connect(pypi, 443).Allowed {
		t.Error("connection to an address never looked up was allowed")
	}
	c.Remember("pypi.org", pypi)
	c.Remember("github.com", gh)
	if d := c.Connect(pypi, 443); !d.Allowed || d.Host != "pypi.org" {
		t.Errorf("pypi.org after lookup: %+v", d)
	}
	if !c.Connect(gh, 443).Allowed || c.Connect(gh, 22).Allowed {
		t.Error("github.com:443 rule not port-limited")
	}
}

// fakeResolver answers from a table.
func fakeResolver(table map[string][]string) lookupFunc {
	return func(_ context.Context, name string) ([]netip.Addr, error) {
		ips, ok := table[name]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
		}
		var out []netip.Addr
		for _, s := range ips {
			out = append(out, netip.MustParseAddr(s))
		}
		return out, nil
	}
}

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) emit(e Event) { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() }

func query(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET})
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func parseReply(t *testing.T, b []byte) (dnsmessage.Header, []netip.Addr) {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		t.Fatalf("bad reply: %v", err)
	}
	var addrs []netip.Addr
	for _, a := range m.Answers {
		if r, ok := a.Body.(*dnsmessage.AResource); ok {
			addrs = append(addrs, netip.AddrFrom4(r.A))
		}
	}
	return m.Header, addrs
}

func TestDNS(t *testing.T) {
	rec := &recorder{}
	c := mustCompile(t, Policy{Mode: ModeAllow, Allow: []string{"pypi.org", "rebind.example"}})
	p := New(c, rec.emit)
	p.lookup = fakeResolver(map[string][]string{
		"pypi.org":       {"151.101.0.223", "2a04:4e42::223"},
		"evil.com":       {"6.6.6.6"},
		"rebind.example": {"192.168.1.1", "127.0.0.1"},
	})

	h, addrs := parseReply(t, p.answerDNS(query(t, "PyPI.org.", dnsmessage.TypeA)))
	if h.ID != 0x1234 || !h.Response || h.RCode != dnsmessage.RCodeSuccess || len(addrs) != 1 ||
		addrs[0] != netip.MustParseAddr("151.101.0.223") {
		t.Errorf("pypi.org: %+v %v", h, addrs)
	}
	if !c.Connect(netip.MustParseAddr("151.101.0.223"), 443).Allowed {
		t.Error("answered address not remembered")
	}
	if h, addrs := parseReply(t, p.answerDNS(query(t, "pypi.org.", dnsmessage.TypeAAAA))); h.RCode != 0 || len(addrs) != 0 {
		t.Errorf("AAAA should be an empty success: %+v", h)
	}
	if h, _ := parseReply(t, p.answerDNS(query(t, "evil.com.", dnsmessage.TypeA))); h.RCode != dnsmessage.RCodeNameError {
		t.Errorf("name off the allow list resolved: %+v", h)
	}
	// A name that resolves only to local addresses is refused, not answered.
	if h, addrs := parseReply(t, p.answerDNS(query(t, "rebind.example.", dnsmessage.TypeA))); h.RCode != dnsmessage.RCodeNameError || len(addrs) != 0 {
		t.Errorf("rebinding answer passed: %+v %v", h, addrs)
	}
	// Garbage and responses are ignored.
	for _, b := range [][]byte{nil, {1, 2, 3}, make([]byte, 12)} {
		if r := p.answerDNS(b); r != nil {
			if _, err := (&dnsmessage.Parser{}).Start(r); err != nil {
				t.Errorf("garbage produced a malformed reply")
			}
		}
	}
	if len(rec.events) == 0 {
		t.Error("no events logged")
	}
}

func FuzzDNS(f *testing.F) {
	c, err := Compile(Policy{Mode: ModeInternet})
	if err != nil {
		f.Fatal(err)
	}
	p := New(c, func(Event) {})
	p.lookup = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("offline") }
	f.Add([]byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 3, 'f', 'o', 'o', 0, 0, 1, 0, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		p.answerDNS(b)
	})
}
