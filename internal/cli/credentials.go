package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/term"

	"github.com/syntaxroot-cc/grsync/internal/daemon"
)

// resolveUser picks the username to authenticate an rsync:// connection
// as: the URL's own "user@" part if it had one, else the USER environment
// variable, else LOGNAME (USER wins if both are set) - matching real
// rsync's own documented resolution exactly (see rsync.1's "USER or
// LOGNAME" section). An empty result is valid: DialAuth itself defaults
// that to "nobody", the same final fallback real rsync uses.
func resolveUser(urlUser string) string {
	if urlUser != "" {
		return urlUser
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return os.Getenv("LOGNAME")
}

// resolvePassword returns a daemon.PasswordFunc for an rsync:// daemon
// connection, matching real rsync's own precedence: --password-file (if
// given) beats the RSYNC_PASSWORD environment variable, which beats an
// interactive terminal prompt (see rsync.1's "RSYNC_PASSWORD" and
// "--password-file" sections). The result is memoized with sync.Once, so
// a multi-source sync against the same daemon destination resolves (and,
// if it comes to it, prompts) at most once - not once per source, even
// though each source gets its own connection (see syncToRsyncDaemon). The
// returned func still only actually runs any of this if DialAuth calls
// it, which only happens if the server challenges for a password at all.
func resolvePassword(passwordFile string, stdin io.Reader) daemon.PasswordFunc {
	var once sync.Once
	var password string
	var resolveErr error

	return func() (string, error) {
		once.Do(func() {
			password, resolveErr = doResolvePassword(passwordFile, stdin)
		})
		return password, resolveErr
	}
}

func doResolvePassword(passwordFile string, stdin io.Reader) (string, error) {
	if passwordFile != "" {
		return readPasswordFile(passwordFile, stdin)
	}
	if password, ok := os.LookupEnv("RSYNC_PASSWORD"); ok {
		return password, nil
	}
	return promptForPassword(stdin)
}

// readPasswordFile reads the first line of path as the password, matching
// real rsync's own --password-file behavior exactly: "-" means read from
// stdin instead of a named file, and the file's contents past the first
// line are ignored (real rsync documents this as deliberate, not a
// limitation - it means a trailing newline, or even a second unrelated
// line, is harmless). A named file is checked against
// checkPasswordFilePermissions first; "-" (stdin) is not, since there is
// no file to check permissions on.
func readPasswordFile(path string, stdin io.Reader) (string, error) {
	var r io.Reader
	if path == "-" {
		r = stdin
	} else {
		if err := checkPasswordFilePermissions(path); err != nil {
			return "", err
		}
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("opening password file: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}

	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("reading password file: %w", err)
		}
		return "", fmt.Errorf("password file is empty")
	}
	return scanner.Text(), nil
}

// promptForPassword shows an interactive, non-echoing "Password: " prompt
// and reads one line, matching real rsync's own getpass()-based fallback
// when neither --password-file nor RSYNC_PASSWORD is set. If stdin isn't
// an interactive terminal (piped input, a test harness, a non-interactive
// script), this returns an empty password rather than prompting or
// erroring - matching real rsync's own behavior when getpass() can't open
// a controlling terminal (it fails silently and rsync falls back to an
// empty password, which then simply fails authentication cleanly if the
// module actually needs one, rather than hanging forever waiting for
// input that will never come).
func promptForPassword(stdin io.Reader) (string, error) {
	f, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", nil
	}

	fmt.Fprint(os.Stderr, "Password: ")
	passwordBytes, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return string(passwordBytes), nil
}
