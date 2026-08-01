//go:build windows

package cli

// checkPasswordFilePermissions is a no-op on Windows: os.FileInfo.Mode()
// there doesn't expose POSIX world-readable permission bits (Go's Windows
// port only reflects the read-only attribute), so there's nothing
// meaningful to check - the same platform split already established for
// ownership and hard-link handling in internal/sync. See
// passwordfile_unix.go for the real check.
func checkPasswordFilePermissions(_ string) error {
	return nil
}
