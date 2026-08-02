package daemon

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/syntaxroot-cc/grsync/internal/pipeline"
	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// startTestDaemon listens on 127.0.0.1:0 (an OS-assigned free port) and
// serves cfg in the background until the test ends. Using a real TCP
// listener rather than io.Pipe is deliberate here: unlike the SSH
// transport tests, this needs no external binary or environment
// dependency (no sshd), so there's no reason to settle for anything less
// than a real end-to-end loopback round trip.
func startTestDaemon(t *testing.T, cfg *Config) (addr string, errLog *bytes.Buffer) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening on loopback: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	errLog = &bytes.Buffer{}
	go func() { _ = Serve(ln, cfg, errLog) }()

	return ln.Addr().String(), errLog
}

func dialTestDaemon(t *testing.T, addr string) *conn {
	t.Helper()
	nc, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialing %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	return newConn(nc)
}

func TestDaemon_RealTCP_AnonymousDownload(t *testing.T) {
	modRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(modRoot, "readme.txt"), "served over a real TCP loopback connection")

	cfg := &Config{Modules: map[string]Module{
		"public": {Name: "public", Path: modRoot, ReadOnly: true, List: true},
	}}
	addr, errLog := startTestDaemon(t, cfg)

	client := dialTestDaemon(t, addr)
	if _, err := DialGreeting(client, "public"); err != nil {
		t.Fatalf("DialGreeting: %v", err)
	}
	if err := DialAuth(client, "", StaticPassword("")); err != nil {
		t.Fatalf("DialAuth: %v", err)
	}
	dest := t.TempDir()
	if err := DialModule(client, DirectionGet, dest, nil, sync.WalkOptions{}, sync.AttrOptions{}, pipeline.ReceiverOptions{}, pipeline.CompressOptions{}); err != nil {
		t.Fatalf("DialModule: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "readme.txt"))
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if string(got) != "served over a real TCP loopback connection" {
		t.Errorf("downloaded content = %q, want the module's file content", got)
	}
	if errLog.Len() != 0 {
		t.Errorf("daemon error log = %q, want empty", errLog.String())
	}
}

func TestDaemon_RealTCP_AuthenticatedUpload(t *testing.T) {
	modRoot := t.TempDir()
	secretsPath := writeSecretsFile(t, "alice:hunter2\n")

	cfg := &Config{Modules: map[string]Module{
		"incoming": {
			Name: "incoming", Path: modRoot, ReadOnly: false,
			AuthUsers: []string{"alice"}, SecretsFile: secretsPath,
		},
	}}
	addr, errLog := startTestDaemon(t, cfg)

	client := dialTestDaemon(t, addr)
	if _, err := DialGreeting(client, "incoming"); err != nil {
		t.Fatalf("DialGreeting: %v", err)
	}
	if err := DialAuth(client, "alice", StaticPassword("hunter2")); err != nil {
		t.Fatalf("DialAuth: %v", err)
	}

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "upload.txt"), "pushed over real TCP")
	rules, err := sync.CompileRules(nil)
	if err != nil {
		t.Fatalf("compiling empty rule set: %v", err)
	}
	if err := DialModule(client, DirectionPut, src, rules, sync.WalkOptions{}, sync.AttrOptions{}, pipeline.ReceiverOptions{}, pipeline.CompressOptions{}); err != nil {
		t.Fatalf("DialModule: %v", err)
	}

	t.Logf("daemon error log: %q", errLog.String())
	got, err := os.ReadFile(filepath.Join(modRoot, "upload.txt"))
	if err != nil {
		t.Fatalf("reading uploaded file from module: %v", err)
	}
	if string(got) != "pushed over real TCP" {
		t.Errorf("uploaded content = %q, want %q", got, "pushed over real TCP")
	}
	if errLog.Len() != 0 {
		t.Errorf("daemon error log = %q, want empty", errLog.String())
	}
}

func TestDaemon_RealTCP_WrongPasswordRejected(t *testing.T) {
	modRoot := t.TempDir()
	secretsPath := writeSecretsFile(t, "alice:hunter2\n")

	cfg := &Config{Modules: map[string]Module{
		"private": {
			Name: "private", Path: modRoot, ReadOnly: true,
			AuthUsers: []string{"alice"}, SecretsFile: secretsPath,
		},
	}}
	addr, _ := startTestDaemon(t, cfg)

	client := dialTestDaemon(t, addr)
	if _, err := DialGreeting(client, "private"); err != nil {
		t.Fatalf("DialGreeting: %v", err)
	}
	if err := DialAuth(client, "alice", StaticPassword("wrong")); err == nil {
		t.Fatalf("DialAuth with a wrong password returned nil error, want an error")
	}
}

func TestDaemon_RealTCP_ModuleListing(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{
		"visible": {Name: "visible", Path: t.TempDir(), ReadOnly: true, List: true, Comment: "shown"},
		"hidden":  {Name: "hidden", Path: t.TempDir(), ReadOnly: true, List: false},
	}}
	addr, _ := startTestDaemon(t, cfg)

	client := dialTestDaemon(t, addr)
	listing, err := DialGreeting(client, "")
	if err != nil {
		t.Fatalf("DialGreeting (listing): %v", err)
	}

	joined := ""
	for _, l := range listing {
		joined += l + "\n"
	}
	if !bytes.Contains([]byte(joined), []byte("visible")) {
		t.Errorf("listing = %q, want it to contain the visible module", joined)
	}
	if bytes.Contains([]byte(joined), []byte("hidden")) {
		t.Errorf("listing = %q, want it to NOT contain the hidden module", joined)
	}
}

// TestDaemon_RealTCP_DryRunPutMakesNoChanges is the daemon protocol's
// real proof for the dry-run wire extension: a DirectionPut with
// ReceiverOptions.DryRun set sends "put --dry-run" on the direction line
// (see dryRunToken), and the module's directory - where ServeModule's
// Receiver actually runs - must stay completely empty afterward, over an
// actual TCP connection, not just a same-process pipe.
func TestDaemon_RealTCP_DryRunPutMakesNoChanges(t *testing.T) {
	modRoot := t.TempDir()
	cfg := &Config{Modules: map[string]Module{
		"incoming": {Name: "incoming", Path: modRoot, ReadOnly: false},
	}}
	addr, errLog := startTestDaemon(t, cfg)

	client := dialTestDaemon(t, addr)
	if _, err := DialGreeting(client, "incoming"); err != nil {
		t.Fatalf("DialGreeting: %v", err)
	}
	if err := DialAuth(client, "", StaticPassword("")); err != nil {
		t.Fatalf("DialAuth: %v", err)
	}

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "upload.txt"), "should never be written to the module")
	rules, err := sync.CompileRules(nil)
	if err != nil {
		t.Fatalf("compiling empty rule set: %v", err)
	}
	ropts := pipeline.ReceiverOptions{DryRun: true}
	if err := DialModule(client, DirectionPut, src, rules, sync.WalkOptions{}, sync.AttrOptions{}, ropts, pipeline.CompressOptions{}); err != nil {
		t.Fatalf("DialModule: %v", err)
	}

	entries, err := os.ReadDir(modRoot)
	if err != nil {
		t.Fatalf("ReadDir(modRoot): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("module directory is not empty after a dry-run put: %v", entries)
	}
	if errLog.Len() != 0 {
		t.Errorf("daemon error log = %q, want empty", errLog.String())
	}
}

// TestDaemon_RealTCP_DryRunGetMakesNoChanges is DryRun's counterpart for
// DirectionGet: unlike PUT, this needs no protocol extension at all - the
// client's own Receiver runs locally here, so ReceiverOptions.DryRun is
// simply consulted directly, the same as a local sync - but it's worth
// proving over a real daemon connection too, not just assumed from the
// PUT case working.
func TestDaemon_RealTCP_DryRunGetMakesNoChanges(t *testing.T) {
	modRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(modRoot, "readme.txt"), "should never be downloaded")

	cfg := &Config{Modules: map[string]Module{
		"public": {Name: "public", Path: modRoot, ReadOnly: true, List: true},
	}}
	addr, errLog := startTestDaemon(t, cfg)

	client := dialTestDaemon(t, addr)
	if _, err := DialGreeting(client, "public"); err != nil {
		t.Fatalf("DialGreeting: %v", err)
	}
	if err := DialAuth(client, "", StaticPassword("")); err != nil {
		t.Fatalf("DialAuth: %v", err)
	}

	dest := t.TempDir()
	ropts := pipeline.ReceiverOptions{DryRun: true}
	if err := DialModule(client, DirectionGet, dest, nil, sync.WalkOptions{}, sync.AttrOptions{}, ropts, pipeline.CompressOptions{}); err != nil {
		t.Fatalf("DialModule: %v", err)
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("ReadDir(dest): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("destination is not empty after a dry-run get: %v", entries)
	}
	if errLog.Len() != 0 {
		t.Errorf("daemon error log = %q, want empty", errLog.String())
	}
}

// TestDaemon_RealTCP_StatsWorkForGet confirms Stats works fully for a
// module download, over a real TCP connection: DirectionGet's Receiver
// runs on the client, exactly like a local sync, so ReceiverOptions
// reaches it directly with no protocol involvement at all - unlike
// DirectionPut, there is no daemon-specific limitation here to disclose.
func TestDaemon_RealTCP_StatsWorkForGet(t *testing.T) {
	modRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(modRoot, "readme.txt"), "some real content to report stats about")

	cfg := &Config{Modules: map[string]Module{
		"public": {Name: "public", Path: modRoot, ReadOnly: true, List: true},
	}}
	addr, _ := startTestDaemon(t, cfg)

	client := dialTestDaemon(t, addr)
	if _, err := DialGreeting(client, "public"); err != nil {
		t.Fatalf("DialGreeting: %v", err)
	}
	if err := DialAuth(client, "", StaticPassword("")); err != nil {
		t.Fatalf("DialAuth: %v", err)
	}

	dest := t.TempDir()
	var out bytes.Buffer
	ropts := pipeline.ReceiverOptions{Stats: true, Output: &out}
	if err := DialModule(client, DirectionGet, dest, nil, sync.WalkOptions{}, sync.AttrOptions{}, ropts, pipeline.CompressOptions{}); err != nil {
		t.Fatalf("DialModule: %v", err)
	}

	output := out.String()
	for _, want := range []string{"Number of files:", "Total file size:", "speedup is"} {
		if !bytes.Contains([]byte(output), []byte(want)) {
			t.Errorf("stats output = %q, want it to contain %q", output, want)
		}
	}
}

// TestDaemon_RealTCP_PutIgnoresProgressAndStatsButStillWorks confirms
// the disclosed daemon-PUT limitation fails safely rather than breaking
// anything: even if a caller sets Progress/Stats on the *client's*
// ReceiverOptions for a DirectionPut, the transfer still completes
// correctly, because those fields never cross the wire at all - only
// DryRun does, via dryRunToken (see ServeModule/DialModule's own doc
// comments) - so the server-side Receiver that would actually need them
// never even sees them. This is what "consistent, not a new gap" means
// concretely: the same fields SC-11 already established as inert for
// daemon-PUT stay inert for Progress/Stats too, without erroring.
func TestDaemon_RealTCP_PutIgnoresProgressAndStatsButStillWorks(t *testing.T) {
	modRoot := t.TempDir()
	cfg := &Config{Modules: map[string]Module{
		"incoming": {Name: "incoming", Path: modRoot, ReadOnly: false},
	}}
	addr, errLog := startTestDaemon(t, cfg)

	client := dialTestDaemon(t, addr)
	if _, err := DialGreeting(client, "incoming"); err != nil {
		t.Fatalf("DialGreeting: %v", err)
	}
	if err := DialAuth(client, "", StaticPassword("")); err != nil {
		t.Fatalf("DialAuth: %v", err)
	}

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "upload.txt"), "pushed despite requesting progress/stats")
	rules, err := sync.CompileRules(nil)
	if err != nil {
		t.Fatalf("compiling empty rule set: %v", err)
	}

	// Progress/Stats set here deliberately, to prove they're harmlessly
	// ignored for this direction rather than causing an error.
	var clientSideOutput bytes.Buffer
	ropts := pipeline.ReceiverOptions{Progress: true, Stats: true, Output: &clientSideOutput}
	if err := DialModule(client, DirectionPut, src, rules, sync.WalkOptions{}, sync.AttrOptions{}, ropts, pipeline.CompressOptions{}); err != nil {
		t.Fatalf("DialModule: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(modRoot, "upload.txt"))
	if err != nil {
		t.Fatalf("reading uploaded file from module: %v", err)
	}
	if string(got) != "pushed despite requesting progress/stats" {
		t.Errorf("uploaded content = %q, want %q", got, "pushed despite requesting progress/stats")
	}
	// Nothing was ever printed on the client side either: DialModule's
	// DirectionPut branch runs pipeline.Sender, which never consults
	// ReceiverOptions at all (see pipeline.Sender's own doc comment) -
	// there is no client-side reporting output for an upload regardless
	// of transport.
	if clientSideOutput.Len() != 0 {
		t.Errorf("client-side output = %q, want empty (Sender never reports progress/stats)", clientSideOutput.String())
	}
	if errLog.Len() != 0 {
		t.Errorf("daemon error log = %q, want empty", errLog.String())
	}
}

// TestDaemon_RealTCP_PutWithCompressUploadsCorrectly is SC-9's real,
// over-the-wire proof for the daemon transport: DirectionPut runs
// pipeline.Sender on the client side (see DialModule's own doc comment),
// exactly where --compress/-z's decision belongs, so this drives that
// same client-side Sender with CompressOptions.Enabled against a real
// TCP daemon connection and confirms the upload still arrives byte-
// correct - the server's Receiver only ever reacts to each deltaMessage's
// own Compressed marker, needing no daemon-protocol change at all.
func TestDaemon_RealTCP_PutWithCompressUploadsCorrectly(t *testing.T) {
	modRoot := t.TempDir()
	cfg := &Config{Modules: map[string]Module{
		"incoming": {Name: "incoming", Path: modRoot, ReadOnly: false},
	}}
	addr, errLog := startTestDaemon(t, cfg)

	client := dialTestDaemon(t, addr)
	if _, err := DialGreeting(client, "incoming"); err != nil {
		t.Fatalf("DialGreeting: %v", err)
	}
	if err := DialAuth(client, "", StaticPassword("")); err != nil {
		t.Fatalf("DialAuth: %v", err)
	}

	src := t.TempDir()
	content := "compressible daemon upload content, repeated. " +
		"compressible daemon upload content, repeated. " +
		"compressible daemon upload content, repeated."
	mustWriteFile(t, filepath.Join(src, "upload.txt"), content)
	rules, err := sync.CompileRules(nil)
	if err != nil {
		t.Fatalf("compiling empty rule set: %v", err)
	}

	copts := pipeline.CompressOptions{Enabled: true, Level: pipeline.DefaultCompressLevel}
	if err := DialModule(client, DirectionPut, src, rules, sync.WalkOptions{}, sync.AttrOptions{}, pipeline.ReceiverOptions{}, copts); err != nil {
		t.Fatalf("DialModule: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(modRoot, "upload.txt"))
	if err != nil {
		t.Fatalf("reading uploaded file from module: %v", err)
	}
	if string(got) != content {
		t.Errorf("uploaded content = %q, want %q", got, content)
	}
	if errLog.Len() != 0 {
		t.Errorf("daemon error log = %q, want empty", errLog.String())
	}
}
