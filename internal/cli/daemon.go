package cli

import (
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/syntaxroot-cc/grsync/internal/daemon"
)

// runDaemon implements --daemon mode: parse the rsyncd.conf at
// opts.config, listen on opts.port, and serve connections until the
// listener fails (e.g. the process is killed) or Accept itself errors.
//
// opts.address ("" by default) is real rsync's own documented daemon-mode
// --address scope exactly: an empty address binds the wildcard address
// (every interface), matching the pre-SC-14 behavior byte-for-byte;
// net.Listen resolves a hostname here itself, so unlike the client-dial
// side (resolveLocalAddr, netopts.go) there's no separate resolution step
// needed. opts.ipv4/opts.ipv6 select "tcp4"/"tcp6" over the default
// dual-stack "tcp" (see tcpNetwork).
func runDaemon(cmd *cobra.Command, opts *options) error {
	if opts.config == "" {
		return fmt.Errorf("--daemon requires --config PATH")
	}

	f, err := os.Open(opts.config)
	if err != nil {
		return fmt.Errorf("opening config %q: %w", opts.config, err)
	}
	cfg, parseErr := daemon.ParseConfig(f)
	closeErr := f.Close()
	if parseErr != nil {
		return fmt.Errorf("parsing config %q: %w", opts.config, parseErr)
	}
	if closeErr != nil {
		return closeErr
	}

	network, err := tcpNetwork(opts.ipv4, opts.ipv6)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(opts.address, strconv.Itoa(opts.port))
	ln, err := net.Listen(network, addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	defer func() { _ = ln.Close() }()

	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "grsync daemon listening on %s (%d module(s) configured)\n", ln.Addr(), len(cfg.Modules)); err != nil {
		return err
	}
	return daemon.Serve(ln, cfg, cmd.ErrOrStderr())
}
