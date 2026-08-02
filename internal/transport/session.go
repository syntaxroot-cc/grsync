package transport

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Session wraps a running remote-shell subprocess (e.g. ssh), exposing
// its stdin/stdout as a single Read/Write pair so the framed protocol can
// be layered directly on top.
type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *bytes.Buffer
}

// Dial spawns the remote-shell command built by BuildRSHCommand and
// returns a Session wrapping its stdin/stdout. The subprocess's stderr is
// captured (to enrich Close's error on a non-zero exit) and also passed
// through live to this process's own stderr.
//
// Host-key verification is left entirely to the invoked command (ssh by
// default); no StrictHostKeyChecking or known_hosts flags are added here.
func Dial(rsh, user, host string, remoteArgs []string, ipv4, ipv6 bool) (*Session, error) {
	argv := BuildRSHCommand(rsh, user, host, remoteArgs, ipv4, ipv6)
	cmd := exec.Command(argv[0], argv[1:]...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		// Start is never reached on this path, so stdin's pipe won't be
		// closed automatically by cmd.Wait; close it explicitly.
		_ = stdin.Close()
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)

	if err := cmd.Start(); err != nil {
		// Start failing here already closes both pipes; nothing to clean up.
		return nil, fmt.Errorf("starting %q: %w", argv[0], err)
	}

	return &Session{cmd: cmd, stdin: stdin, stdout: stdout, stderr: &stderr}, nil
}

// Read reads from the subprocess's stdout.
func (s *Session) Read(p []byte) (int, error) { return s.stdout.Read(p) }

// Write writes to the subprocess's stdin.
func (s *Session) Write(p []byte) (int, error) { return s.stdin.Write(p) }

// Close closes the subprocess's stdin (signaling EOF to the remote side)
// and waits for it to exit; stdout is left for cmd.Wait to close itself.
// If the process exited with an error, stderr output is appended for a
// more useful message than a bare exit code.
func (s *Session) Close() error {
	stdinErr := s.stdin.Close()
	waitErr := s.cmd.Wait()

	if waitErr != nil {
		if msg := strings.TrimSpace(s.stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", waitErr, msg)
		}
		return waitErr
	}
	return stdinErr
}
