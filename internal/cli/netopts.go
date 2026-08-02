package cli

import (
	"fmt"
	"net"
)

// tcpNetwork returns the net.Dial/net.Listen network string ("tcp",
// "tcp4", or "tcp6") implied by --ipv4/--ipv6, or an error if both were
// somehow given together.
//
// Real rsync's own popt registration lets -4 and -6 write to the very
// same C variable (default_af_hint, verified against upstream's
// options.c), so whichever one is parsed last on the command line
// silently wins if both are given - not a designed behavior, just an
// artifact of how those two options happen to be wired up in C. grsync
// treats the combination as a clear, explicit error instead: an
// arbitrary "whichever came last" outcome is worse than a prompt,
// understandable one, and every real rsync document describing these
// flags (the man page included) presents them as alternatives, never as
// a valid pair to combine.
func tcpNetwork(ipv4, ipv6 bool) (string, error) {
	switch {
	case ipv4 && ipv6:
		return "", fmt.Errorf("--ipv4 and --ipv6 are mutually exclusive")
	case ipv4:
		return "tcp4", nil
	case ipv6:
		return "tcp6", nil
	default:
		return "tcp", nil
	}
}

// resolveLocalAddr resolves address (an --address value: a literal IP or
// a hostname, matching real rsync's own documented "IP address (or
// hostname)" scope for its client-side --address) into a concrete local
// address to bind an outbound connection's source address to, honoring
// network ("tcp4"/"tcp6"/"tcp") the same way the connection itself is
// about to be dialed - so --address combined with --ipv6 resolves a
// hostname to its IPv6 address specifically, not whichever family
// happens to come back first.
//
// Returns (nil, nil) for an empty address: "no local address
// preference," the default before --address existed at all, and exactly
// what net.Dialer.LocalAddr being left as its own zero value already
// means.
func resolveLocalAddr(network, address string) (*net.TCPAddr, error) {
	if address == "" {
		return nil, nil
	}
	addr, err := net.ResolveTCPAddr(network, net.JoinHostPort(address, "0"))
	if err != nil {
		return nil, fmt.Errorf("resolving --address %q: %w", address, err)
	}
	return addr, nil
}
