package cli

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/syntaxroot-cc/grsync/internal/daemon"
	"github.com/syntaxroot-cc/grsync/internal/pipeline"
	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// isRsyncURL reports whether s looks like an rsync:// daemon URL, as
// opposed to a local path or an SSH user@host:path. Checked by prefix
// (rather than by trying daemon.ParseURL and inspecting the error) so a
// malformed rsync:// URL is reported as a clear parse error from
// daemon.ParseURL itself, instead of silently falling through to be
// treated as some other kind of argument.
func isRsyncURL(s string) bool {
	return strings.HasPrefix(s, "rsync://")
}

// dialDaemonTimeout bounds how long connecting to an rsync:// daemon can
// take, so an unreachable or non-responding host fails with a clear error
// instead of hanging the whole command indefinitely.
const dialDaemonTimeout = 10 * time.Second

// syncToRsyncDaemon uploads src to an rsync:// daemon destination u:
// dials the daemon over plain TCP, then hands the connection straight to
// daemon.DialClient, which runs the real handshake/authentication and
// then the same pipeline.Sender every other destination uses - this
// function's only job is to get from a URL to a net.Conn and supply the
// credentials, not to know anything about the transfer itself. hardLinks
// is the only AttrOptions field DialClient's Sender-side (DirectionPut)
// call actually consults, but it's threaded through as a full
// sync.AttrOptions to match DialClient's own signature.
//
// dryRun is the only ReceiverOptions field that reaches the daemon at
// all for this direction: the module's Receiver runs on the server, not
// here, so DialModule sends it as an extra token on the wire (see
// daemon.dryRunToken) rather than anything this function does directly.
// Itemize/Verbose are deliberately not passed through - see runSync's
// own one-time note about why daemon-PUT reporting output isn't
// available, printed before this function is ever called.
func syncToRsyncDaemon(src string, u daemon.URL, password daemon.PasswordFunc, walkOpts sync.WalkOptions, rules []sync.Rule, hardLinks bool, dryRun bool) error {
	port := u.Port
	if port == 0 {
		port = daemon.DefaultPort
	}
	addr := net.JoinHostPort(u.Host, strconv.Itoa(port))

	nc, err := net.DialTimeout("tcp", addr, dialDaemonTimeout)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", addr, err)
	}
	defer func() { _ = nc.Close() }()

	user := resolveUser(u.User)
	ropts := pipeline.ReceiverOptions{DryRun: dryRun}
	return daemon.DialClient(nc, u.Module, user, password, daemon.DirectionPut, src, rules, walkOpts, sync.AttrOptions{HardLinks: hardLinks}, ropts)
}
