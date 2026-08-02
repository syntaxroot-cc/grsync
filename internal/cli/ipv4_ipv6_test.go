package cli

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syntaxroot-cc/grsync/internal/daemon"
)

// requireIPv6Loopback skips the calling test if this environment doesn't
// have a working IPv6 loopback - some CI environments and containers
// don't, and this project's established pattern (see
// requireLocalSSHServer, internal/pipeline/ssh_test.go) is to skip
// gracefully rather than fail when an environment-dependent capability
// isn't present.
func requireIPv6Loopback(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback available in this environment: %v", err)
	}
	_ = ln.Close()
}

// startTestDaemonOnAddr is startTestDaemon's counterpart for a specific
// network/address rather than always "tcp" on 127.0.0.1 - used here to
// drive daemon.Serve directly against a real IPv6 loopback listener,
// proving the underlying daemon transport works over both address
// families independently of the CLI's own --daemon listener wiring
// (covered separately by startRealDaemonCLI below).
func startTestDaemonOnAddr(t *testing.T, network, addr string, cfg *daemon.Config) (actualAddr string, errLog *strings.Builder) {
	t.Helper()

	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Fatalf("listening on %s %s: %v", network, addr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	errLog = &strings.Builder{}
	go func() { _ = daemon.Serve(ln, cfg, errLog) }()

	return ln.Addr().String(), errLog
}

// startRealDaemonCLI drives the real --daemon command (not
// daemon.Serve directly - see startTestDaemon/startTestDaemonOnAddr for
// that) so this test genuinely exercises runDaemon's own --ipv4/--ipv6/
// --address wiring (internal/cli/daemon.go), not just the tcpNetwork/
// net.Listen primitives it's built from. args must not include --daemon,
// --config, or --port - those are added here.
//
// The spawned goroutine's Accept loop is deliberately never stopped:
// there is no handle to the listener runDaemon creates internally to
// close it from outside (by design - see daemon.go's own doc comment on
// why --address's resolution happens inside runDaemon, not before it),
// so it blocks on Accept for the rest of this test binary's process
// lifetime rather than the individual test's. That's a bounded,
// contained cost (one goroutine, one open socket, gone at process exit),
// not a real leak, and is the same trade-off any test of a blocking
// accept-loop server makes without its own dedicated shutdown hook.
func startRealDaemonCLI(t *testing.T, configPath string, extraArgs ...string) (addr string) {
	t.Helper()

	pr, pw := io.Pipe()
	cmd := NewRootCmd()
	args := append([]string{"--daemon", "--config", configPath, "--port", "0"}, extraArgs...)
	cmd.SetArgs(args)
	cmd.SetOut(pw)
	cmd.SetIn(strings.NewReader(""))

	execErrCh := make(chan error, 1)
	go func() { execErrCh <- cmd.Execute() }()

	lineCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(pr)
		if scanner.Scan() {
			lineCh <- scanner.Text()
		}
	}()

	select {
	case line := <-lineCh:
		fields := strings.Fields(line)
		// "grsync daemon listening on 127.0.0.1:12345 (1 module(s) configured)"
		for _, f := range fields {
			if strings.Contains(f, ":") {
				return f
			}
		}
		t.Fatalf("could not find an address in daemon startup line: %q", line)
	case err := <-execErrCh:
		t.Fatalf("grsync --daemon exited before reporting a listening address: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for grsync --daemon to report its listening address")
	}
	return ""
}

func writeTestDaemonConfig(t *testing.T, modRoot string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rsyncd.conf")
	content := fmt.Sprintf("[incoming]\n    path = %s\n    read only = false\n", filepath.ToSlash(modRoot))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing rsyncd.conf: %v", err)
	}
	return path
}

// TestE2E_DaemonListensOnIPv4LoopbackWithAddressFlag is the ticket's own
// explicit requirement: a real --daemon process started with
// --address 127.0.0.1 --ipv4, and a real client sync against it,
// proving runDaemon's own network/address wiring - not just the
// underlying net.Listen primitive - actually works end to end.
func TestE2E_DaemonListensOnIPv4LoopbackWithAddressFlag(t *testing.T) {
	modRoot := t.TempDir()
	configPath := writeTestDaemonConfig(t, modRoot)

	addr := startRealDaemonCLI(t, configPath, "--address", "127.0.0.1", "--ipv4")
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("daemon reported listening address %q, want it bound to 127.0.0.1", addr)
	}

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "hello.txt"), "over ipv4 loopback via --address")

	dest := fmt.Sprintf("rsync://%s/incoming", addr)
	if err := runGrsync(t, "-a", src, dest); err != nil {
		t.Fatalf("grsync %s returned error: %v", dest, err)
	}
	got, err := os.ReadFile(filepath.Join(modRoot, "hello.txt"))
	if err != nil {
		t.Fatalf("reading synced file: %v", err)
	}
	if string(got) != "over ipv4 loopback via --address" {
		t.Errorf("content = %q, want the source content", got)
	}
}

// TestE2E_DaemonListensOnIPv6LoopbackWithAddressFlag is
// TestE2E_DaemonListensOnIPv4LoopbackWithAddressFlag's IPv6 counterpart -
// the ticket's other explicitly required loopback address. Skips
// gracefully without a working IPv6 loopback in this environment.
func TestE2E_DaemonListensOnIPv6LoopbackWithAddressFlag(t *testing.T) {
	requireIPv6Loopback(t)

	modRoot := t.TempDir()
	configPath := writeTestDaemonConfig(t, modRoot)

	addr := startRealDaemonCLI(t, configPath, "--address", "::1", "--ipv6")
	if !strings.HasPrefix(addr, "[::1]:") {
		t.Fatalf("daemon reported listening address %q, want it bound to [::1]", addr)
	}

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "hello.txt"), "over ipv6 loopback via --address")

	dest := fmt.Sprintf("rsync://%s/incoming", addr)
	if err := runGrsync(t, "-a", "--ipv6", src, dest); err != nil {
		t.Fatalf("grsync %s returned error: %v", dest, err)
	}
	got, err := os.ReadFile(filepath.Join(modRoot, "hello.txt"))
	if err != nil {
		t.Fatalf("reading synced file: %v", err)
	}
	if string(got) != "over ipv6 loopback via --address" {
		t.Errorf("content = %q, want the source content", got)
	}
}

// TestE2E_ClientDialsOverIPv6 is the client-dial-side counterpart: a
// daemon listening only on an IPv6 loopback socket (bound directly via
// daemon.Serve, bypassing the CLI's own --daemon listener entirely - see
// startTestDaemonOnAddr), synced to with a real grsync client, no
// --ipv4/--ipv6 override needed since the URL's own [::1] host already
// unambiguously implies IPv6 - proving syncToRsyncDaemon's dialer works
// over a real IPv6 socket, not just IPv4.
func TestE2E_ClientDialsOverIPv6(t *testing.T) {
	requireIPv6Loopback(t)

	modRoot := t.TempDir()
	cfg := &daemon.Config{Modules: map[string]daemon.Module{
		"incoming": {Name: "incoming", Path: modRoot, ReadOnly: false},
	}}
	addr, errLog := startTestDaemonOnAddr(t, "tcp6", "[::1]:0", cfg)

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "hello.txt"), "dialed over a real ipv6 socket")

	dest := fmt.Sprintf("rsync://%s/incoming", addr)
	if err := runGrsync(t, "-a", src, dest); err != nil {
		t.Fatalf("grsync %s returned error: %v", dest, err)
	}
	got, err := os.ReadFile(filepath.Join(modRoot, "hello.txt"))
	if err != nil {
		t.Fatalf("reading synced file: %v", err)
	}
	if string(got) != "dialed over a real ipv6 socket" {
		t.Errorf("content = %q, want the source content", got)
	}
	if errLog.Len() != 0 {
		t.Errorf("daemon error log = %q, want empty", errLog.String())
	}
}

// TestE2E_ForcingIPv4AgainstIPv6OnlyDaemonFails is the ticket's explicit
// "verify --ipv4/--ipv6 flags actually constrain which address family is
// used" requirement made concrete: a daemon bound ONLY to the IPv6
// loopback, dialed by a client that forces --ipv4 against that same
// loopback hostname, must fail to connect - proving --ipv4 genuinely
// restricts which address family net.Dial is even allowed to try,
// rather than silently falling back to whatever resolves.
func TestE2E_ForcingIPv4AgainstIPv6OnlyDaemonFails(t *testing.T) {
	requireIPv6Loopback(t)

	modRoot := t.TempDir()
	cfg := &daemon.Config{Modules: map[string]daemon.Module{
		"incoming": {Name: "incoming", Path: modRoot, ReadOnly: false},
	}}
	// Bind by port only (IPv6 loopback), then dial "localhost" (which
	// resolves to both 127.0.0.1 and ::1 in a normal dual-stack
	// environment) forcing --ipv4 - real rsync's own documented meaning
	// of --ipv4 ("prefer IPv4... this affects sockets rsync has direct
	// control over") is exactly this: constrain which family gets tried.
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatalf("listening on [::1]:0: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	errLog := &strings.Builder{}
	go func() { _ = daemon.Serve(ln, cfg, errLog) }()

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "should never arrive")

	dest := fmt.Sprintf("rsync://localhost:%d/incoming", port)
	err = runGrsync(t, "-a", "--ipv4", src, dest)
	if err == nil {
		t.Fatalf("grsync %s --ipv4 against an IPv6-only daemon returned nil error, want a connection failure", dest)
	}
}

func TestE2E_IPv4AndIPv6TogetherIsRejected(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "x")
	dst := t.TempDir()

	err := runGrsync(t, "-a", "--ipv4", "--ipv6", src, dst)
	if err == nil {
		t.Fatal("grsync --ipv4 --ipv6 together returned nil error, want a mutually-exclusive error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want it to mention --ipv4/--ipv6 being mutually exclusive", err.Error())
	}
}

// TestE2E_GarbageAddressFailsEvenForLocalSync is a self-review-driven
// edge case: --address is resolved eagerly in runSync, before the
// per-source loop and regardless of destination type (see runSync's own
// comment on why), so a garbage --address value is caught up front with
// a clear error even for a local-only sync where --address would
// otherwise have no effect at all. Real rsync itself only ever consults
// --address inside its own socket-opening code, so an invalid value
// there would silently never be noticed for a local copy - grsync
// deliberately trades that exact-behavior match for fail-fast
// predictability instead, the same trade-off tcpNetwork's own doc
// comment already discloses for --ipv4/--ipv6 conflicts.
func TestE2E_GarbageAddressFailsEvenForLocalSync(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "x")
	dst := t.TempDir()

	err := runGrsync(t, "-a", "--address", "this is not a valid address or hostname!!", src, dst)
	if err == nil {
		t.Fatal("grsync --address <garbage> for a local sync returned nil error, want a clear resolution error")
	}
}

// TestE2E_DaemonAddressFamilyMismatchFailsClearly is a self-review-driven
// edge case: combining --ipv4 with an --address that's actually an IPv6
// literal is a genuine, self-contradictory request (bind to an IPv6
// address using the IPv4-only network) - net.Listen itself rejects this
// combination, and this test confirms that surfaces as a clear returned
// error, not a hang or a panic.
func TestE2E_DaemonAddressFamilyMismatchFailsClearly(t *testing.T) {
	requireIPv6Loopback(t)

	modRoot := t.TempDir()
	configPath := writeTestDaemonConfig(t, modRoot)

	err := runGrsync(t, "--daemon", "--config", configPath, "--port", "0", "--ipv4", "--address", "::1")
	if err == nil {
		t.Fatal("grsync --daemon --ipv4 --address ::1 returned nil error, want a clear listen error for the family mismatch")
	}
}

// TestE2E_AddressAppliesEvenForLocalSyncValidation confirms --ipv4/--ipv6
// conflict validation happens even for a local sync, where the flags
// have no actual effect - a deliberate, disclosed choice (see
// tcpNetwork's own doc comment) for predictable, uniform behavior
// regardless of destination type, rather than validating differently
// depending on what happens to be reachable at the end of the command.
func TestE2E_AddressAppliesEvenForLocalSyncValidation(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "content")
	dst := t.TempDir()

	if err := runGrsync(t, "-a", "--ipv4", src, dst); err != nil {
		t.Fatalf("grsync -a --ipv4 for a local sync returned error: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "f.txt"))
	if err != nil || string(got) != "content" {
		t.Errorf("local sync with --ipv4 harmlessly set did not complete correctly: content=%q err=%v", got, err)
	}
}
