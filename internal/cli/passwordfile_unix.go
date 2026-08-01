//go:build !windows

package cli

import (
	"fmt"
	"os"
)

// checkPasswordFilePermissions refuses a world-readable --password-file,
// matching real rsync's own documented behavior: "Rsync will exit with
// an error if FILE is world readable." Real rsync also refuses a
// non-root-owned file when running as root; that narrower check is not
// implemented here.
func checkPasswordFilePermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("checking password file: %w", err)
	}
	if info.Mode().Perm()&0o004 != 0 {
		return fmt.Errorf("password file %q must not be world readable", path)
	}
	return nil
}
