// Command grsync is the CLI entrypoint. It is intentionally thin: all
// command/flag definitions live in internal/cli so they stay testable
// without invoking a real process.
package main

import (
	"fmt"
	"os"

	"github.com/syntaxroot-cc/grsync/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
