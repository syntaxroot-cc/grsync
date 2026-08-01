package daemon

import (
	"errors"
	"fmt"
	"io"
	"net"
)

// ServeConn runs one full connection's daemon protocol end to end: the
// greeting/module-selection handshake, authentication (only performed at
// all if the selected module requires it), and the resulting transfer -
// using only this package's own per-phase functions plus the connection
// itself. A bare module-listing request (ErrModuleListRequested) is a
// normal outcome, not a failure; the connection is simply done at that
// point.
func ServeConn(nc net.Conn, cfg *Config) error {
	c := newConn(nc)

	m, err := ServeGreeting(c, cfg)
	if err != nil {
		return err
	}

	if _, err := ServeAuth(c, m); err != nil {
		return err
	}

	return ServeModule(c, m)
}

// Serve accepts connections on ln until Accept itself fails (typically
// because ln was closed), running ServeConn for each one in its own
// goroutine so one slow or misbehaving client can't block any other.
// Per-connection errors are written to errLog rather than returned -
// Serve's own return only ever reflects the listener itself failing, not
// any individual client's session.
func Serve(ln net.Listener, cfg *Config, errLog io.Writer) error {
	for {
		nc, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("accepting connection: %w", err)
		}

		go func() {
			defer func() { _ = nc.Close() }()
			// A network-facing daemon must not let one connection's bad
			// input (malformed data anywhere downstream in the
			// handshake/auth/transfer chain, not just malformed
			// rsyncd.conf) take the whole process - and therefore every
			// other in-flight connection - down with it. Go doesn't
			// isolate goroutine panics on its own, so this recover is
			// what makes "one client, one goroutine" actually mean one
			// client's failure stays scoped to that client.
			defer func() {
				if r := recover(); r != nil {
					_, _ = fmt.Fprintf(errLog, "%s: panic: %v\n", nc.RemoteAddr(), r)
				}
			}()
			if err := ServeConn(nc, cfg); err != nil && !errors.Is(err, ErrModuleListRequested) {
				_, _ = fmt.Fprintf(errLog, "%s: %v\n", nc.RemoteAddr(), err)
			}
		}()
	}
}
