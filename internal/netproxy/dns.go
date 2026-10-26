package netproxy

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// dnsTTL is the TTL on every answer. Short, so a long-running guest
// re-asks (and is re-checked) often.
const dnsTTL = 30

// lookup resolves a host name to IPv4 addresses on the host.
type lookupFunc func(ctx context.Context, name string) ([]netip.Addr, error)

func hostLookup(ctx context.Context, name string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", name)
}

// answerDNS builds the reply to one query from the guest. It returns nil
// for packets that should be ignored (not a well-formed query).
func (p *Proxy) answerDNS(query []byte) []byte {
	var parser dnsmessage.Parser
	hdr, err := parser.Start(query)
	if err != nil || hdr.Response {
		return nil
	}
	questions, err := parser.AllQuestions()
	if err != nil {
		return nil
	}
	reply := dnsmessage.Header{
		ID: hdr.ID, Response: true, OpCode: hdr.OpCode,
		RecursionDesired: hdr.RecursionDesired, RecursionAvailable: true,
	}
	if hdr.OpCode != 0 || len(questions) != 1 {
		reply.RCode = dnsmessage.RCodeNotImplemented
		return buildDNS(reply, questions, nil)
	}
	q := questions[0]
	name := strings.ToLower(strings.TrimSuffix(q.Name.String(), "."))
	if q.Class != dnsmessage.ClassINET || !validHost(name) {
		reply.RCode = dnsmessage.RCodeRefused
		return buildDNS(reply, questions, nil)
	}
	if !p.check.AllowName(name) {
		if q.Type != dnsmessage.TypeAAAA { // resolvers ask both; log once
			p.emit(Event{Kind: "dns", Name: name, Reason: "name not on the allow list"})
		}
		reply.RCode = dnsmessage.RCodeNameError
		return buildDNS(reply, questions, nil)
	}
	if q.Type != dnsmessage.TypeA {
		// IPv4 only: an empty answer for AAAA and the rest makes resolvers
		// fall back to the A record.
		return buildDNS(reply, questions, nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := p.lookup(ctx, name)
	if err != nil {
		if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
			reply.RCode = dnsmessage.RCodeNameError
		} else {
			reply.RCode = dnsmessage.RCodeServerFailure
		}
		p.emit(Event{Kind: "dns", Name: name, Allowed: true, Reason: "lookup failed: " + err.Error()})
		return buildDNS(reply, questions, nil)
	}
	var answers []netip.Addr
	var dropped int
	for _, a := range addrs {
		a = a.Unmap()
		if !a.Is4() {
			continue
		}
		if !p.check.AllowResolved(a) {
			dropped++
			continue
		}
		p.check.Remember(name, a)
		answers = append(answers, a)
		if len(answers) == 8 {
			break
		}
	}
	ev := Event{Kind: "dns", Name: name, Allowed: true}
	for _, a := range answers {
		ev.Answers = append(ev.Answers, a.String())
	}
	if dropped > 0 {
		ev.Reason = "dropped private or local addresses from the answer"
		if len(answers) == 0 {
			ev.Allowed = false
			reply.RCode = dnsmessage.RCodeNameError
		}
	}
	p.emit(ev)
	return buildDNS(reply, questions, answers)
}

func buildDNS(h dnsmessage.Header, questions []dnsmessage.Question, answers []netip.Addr) []byte {
	b := dnsmessage.NewBuilder(make([]byte, 0, 512), h)
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil
	}
	for _, q := range questions {
		if b.Question(q) != nil {
			return nil
		}
	}
	if err := b.StartAnswers(); err != nil {
		return nil
	}
	for _, a := range answers {
		rh := dnsmessage.ResourceHeader{Name: questions[0].Name, Class: dnsmessage.ClassINET, TTL: dnsTTL}
		if b.AResource(rh, dnsmessage.AResource{A: a.As4()}) != nil {
			return nil
		}
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}
