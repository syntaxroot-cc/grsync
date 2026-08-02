package daemon

import (
	"errors"
	"fmt"
	"io"
	"net"
)

// ServeConn runs one full connection's daemon protocol end to end: the
// greeting/module-selection handshake, authentication (if the selected
// module requires it), and the resulting transfer. A bare module-listing
// request (ErrModuleListRequested) is a normal outcome, not a failure.
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

// Serve accepts connections on ln until Accept fails (typically because ln
// was closed), running ServeConn for each one in its own goroutine so one
// slow or misbehaving client can't block any other. Per-connection errors
// go to errLog; Serve's own return only reflects the listener failing.
func Serve(ln net.Listener, cfg *Config, errLog io.Writer) error {
	for {
		nc, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("accepting connection: %w", err)
		}

		go func() {
			defer func() { _ = nc.Close() }()
			// Go doesn't isolate goroutine panics on its own; without this
			// recover, one client's bad input could take down every other
			// in-flight connection along with it.
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
