package cli

import (
	"fmt"
	"net"
	"os"

	"github.com/spf13/cobra"

	"github.com/syntaxroot-cc/grsync/internal/daemon"
)

// runDaemon implements --daemon mode: parse the rsyncd.conf at configPath,
// listen on port, and serve connections until the listener fails (e.g. the
// process is killed) or Accept itself errors.
func runDaemon(cmd *cobra.Command, configPath string, port int) error {
	if configPath == "" {
		return fmt.Errorf("--daemon requires --config PATH")
	}

	f, err := os.Open(configPath)
	if err != nil {
		return fmt.Errorf("opening config %q: %w", configPath, err)
	}
	cfg, parseErr := daemon.ParseConfig(f)
	closeErr := f.Close()
	if parseErr != nil {
		return fmt.Errorf("parsing config %q: %w", configPath, parseErr)
	}
	if closeErr != nil {
		return closeErr
	}

	addr := fmt.Sprintf(":%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	defer func() { _ = ln.Close() }()

	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "grsync daemon listening on %s (%d module(s) configured)\n", ln.Addr(), len(cfg.Modules)); err != nil {
		return err
	}
	return daemon.Serve(ln, cfg, cmd.ErrOrStderr())
}
