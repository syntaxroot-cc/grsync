package transport

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Session wraps a running remote-shell subprocess (e.g. ssh), exposing
// its stdin/stdout as a single Read/Write pair so the framed protocol
// (frame.go) can be layered directly on top without the caller needing
// to know this is a subprocess at all.
type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *bytes.Buffer
}

// Dial spawns the remote-shell command built by BuildRSHCommand (ssh, or
// whatever --rsh/-e overrides it to) and returns a Session wrapping its
// stdin/stdout. The subprocess's stderr is captured (not connected to
// this process's own stderr) so it can be surfaced as part of a
// meaningful error from Close if the process exits non-zero.
//
// Host-key verification is never touched here: this deliberately never
// adds flags like "-o StrictHostKeyChecking=no" or a null
// UserKnownHostsFile. Whatever the invoked command (ssh by default) does
// by default - checking known_hosts, prompting or failing on an unknown
// or changed host key - is exactly what happens, unmodified. There is no
// stubbed-out or weakened host-key behavior to document here because none
// of that logic is reimplemented at all; it's entirely the system ssh
// client's own, unchanged behavior.
func Dial(rsh, user, host string, remoteArgs []string) (*Session, error) {
	argv := BuildRSHCommand(rsh, user, host, remoteArgs)
	cmd := exec.Command(argv[0], argv[1:]...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		// StdinPipe already succeeded above, and Start is never reached
		// on this path - so nothing will ever close that pipe's file
		// descriptor automatically (that's normally Cmd.Wait's job, and
		// Wait only does it once Start has actually run). Verified
		// against golang/go#58369, which documents this exact gap: Start
		// itself failing *does* clean up already-created pipes, but never
		// reaching Start at all does not. Close stdin explicitly here
		// rather than leak it.
		_ = stdin.Close()
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		// Unlike the StdoutPipe case above, Start failing here *does*
		// close both already-created pipes as part of its own documented
		// cleanup (golang/go#58369) - nothing further to release.
		return nil, fmt.Errorf("starting %q: %w", argv[0], err)
	}

	return &Session{cmd: cmd, stdin: stdin, stdout: stdout, stderr: &stderr}, nil
}

// Read reads from the subprocess's stdout.
func (s *Session) Read(p []byte) (int, error) { return s.stdout.Read(p) }

// Write writes to the subprocess's stdin.
func (s *Session) Write(p []byte) (int, error) { return s.stdin.Write(p) }

// Close closes the subprocess's stdin (signaling EOF to the remote side)
// and waits for it to exit.
//
// stdout is deliberately not closed here: cmd.Wait's own documentation
// states it closes any pipe created via StdoutPipe automatically once the
// command exits, and that closing it earlier is incorrect if reads from
// it haven't all completed yet - so an explicit s.stdout.Close() here
// would either be redundant or actively wrong, depending on timing.
//
// If the process exited with an error, that error is enriched with
// whatever the subprocess wrote to stderr: a bare "exit status 255" is
// nearly useless without the diagnostic message ssh itself printed
// explaining why (unknown host, auth failure, connection refused, ...).
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
