package daemon

import (
	"fmt"
	"net"

	"github.com/syntaxroot-cc/grsync/internal/pipeline"
	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// DialClient runs a full client-side daemon session over an already-
// connected nc: the greeting/module-selection handshake, authentication
// (only actually triggered if the server challenges for it - see
// PasswordFunc), and the resulting transfer against localPath. It is the
// client-side counterpart to ServeConn, and exists for the same reason:
// DialGreeting/DialAuth/DialModule are built around this package's own
// unexported conn type, so external callers (internal/cli, in
// particular) had no way to actually reach them until now - this is
// where net.Conn crosses into that internal representation.
//
// module must be non-empty: DialClient runs a transfer, not a listing -
// callers that want to list a daemon's modules should use DialGreeting
// directly with an empty module instead. See DialModule's own doc
// comment for exactly what ropts does and doesn't reach on each
// direction.
func DialClient(nc net.Conn, module, user string, password PasswordFunc, direction Direction, localPath string, rules []sync.Rule, walkOpts sync.WalkOptions, attrOpts sync.AttrOptions, ropts pipeline.ReceiverOptions) error {
	if module == "" {
		return fmt.Errorf("DialClient requires a module name")
	}

	c := newConn(nc)

	if _, err := DialGreeting(c, module); err != nil {
		return err
	}
	if err := DialAuth(c, user, password); err != nil {
		return err
	}
	return DialModule(c, direction, localPath, rules, walkOpts, attrOpts, ropts)
}
