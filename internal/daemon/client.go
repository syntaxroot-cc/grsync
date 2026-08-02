package daemon

import (
	"fmt"
	"net"

	"github.com/syntaxroot-cc/grsync/internal/pipeline"
	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// DialClient runs a full client-side daemon session over an already-
// connected nc: the greeting/module-selection handshake, authentication,
// and the resulting transfer against localPath. module must be non-empty;
// callers that want to list a daemon's modules should use DialGreeting
// directly with an empty module instead.
func DialClient(nc net.Conn, module, user string, password PasswordFunc, direction Direction, localPath string, rules []sync.Rule, walkOpts sync.WalkOptions, attrOpts sync.AttrOptions, ropts pipeline.ReceiverOptions, copts pipeline.CompressOptions) error {
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
	return DialModule(c, direction, localPath, rules, walkOpts, attrOpts, ropts, copts)
}
