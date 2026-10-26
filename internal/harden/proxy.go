package harden

// Seccomp filter for the network proxy (ksar net-proxy).
//
// The proxy parses the guest's network traffic and holds real sockets on
// the host, so it gets its own allowlist: what the VMM may do (minus all
// ioctls), plus IPv4/IPv6 sockets for outgoing connections and read-only
// file opens for the host resolver (/etc/resolv.conf, /etc/hosts). It
// can't exec, start processes, write or create files, or open Unix,
// netlink or raw sockets.

var proxyAllowed = map[string]uint32{
	"poll": 7, "readv": 19, "writev": 20, "ppoll": 271,
	"connect": 42, "sendto": 44, "recvfrom": 45, "sendmsg": 46, "recvmsg": 47,
	"shutdown": 48, "bind": 49, "getsockname": 51, "getpeername": 52,
	"setsockopt": 54, "getsockopt": 55, "recvmmsg": 299, "sendmmsg": 307,
	"newfstatat": 262, "statx": 332,
	"uname": 63, // the Go resolver reads the hostname for its search list
}

const (
	nrSocket = 41
	nrOpenat = 257

	afInet  = 2
	afInet6 = 10

	// O_ACCMODE plus everything that can create or change a file.
	openWriteFlags = 0x3 | 0x40 /* O_CREAT */ | 0x200 /* O_TRUNC */ | 0x400 /* O_APPEND */ | 0x400000 /* O_TMPFILE */
)

// LockdownProxy restricts the network proxy process. Call it once the
// policy is compiled (which reads the host's interfaces over netlink).
func LockdownProxy() error {
	if err := basic(); err != nil {
		return err
	}
	return installSeccomp(proxyFilter())
}

func proxyFilter() program {
	allow := map[string]uint32{}
	for k, v := range allowed {
		allow[k] = v
	}
	for k, v := range proxyAllowed {
		allow[k] = v
	}
	delete(allow, "accept4") // the proxy never accepts host connections
	return buildFilter(allow, func(p *program) {
		// socket: IPv4 and IPv6 only.
		p.cond(nrSocket, func(b *program) {
			b.ld(argLow(0))
			b.jeq(afInet, 2, 0)
			b.jeq(afInet6, 1, 0)
			b.ret(retTrap)
			b.ret(retAllow)
		})
		// openat: read-only, never creating.
		p.cond(nrOpenat, func(b *program) {
			b.ld(argLow(2))
			b.jset(openWriteFlags, 0, 1)
			b.ret(retTrap)
			b.ret(retAllow)
		})
	})
}
