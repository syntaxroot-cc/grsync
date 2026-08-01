package daemon

import (
	"strings"
	"testing"
)

func TestParseConfig_RealisticSample(t *testing.T) {
	const sample = `
# Global defaults - every module below inherits these unless it overrides them.
read only = yes
list = yes

[public]
	path = /srv/rsync/public
	comment = Public files, read-only, no auth
	exclude = *.tmp *.bak .git/

[private]
	path = /srv/rsync/private
	read only = false
	list = false
	auth users = alice, bob
	secrets file = /etc/rsyncd.secrets
	max connections = 4

[backup]
	path = /srv/rsync/backup \
	       continued path is not realistic but exercises line continuation
`

	cfg, err := ParseConfig(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("ParseConfig returned error: %v", err)
	}

	if len(cfg.Modules) != 3 {
		t.Fatalf("got %d modules, want 3: %+v", len(cfg.Modules), cfg.Modules)
	}

	public, ok := cfg.Modules["public"]
	if !ok {
		t.Fatalf("missing module %q", "public")
	}
	if public.Path != "/srv/rsync/public" {
		t.Errorf("public.Path = %q, want %q", public.Path, "/srv/rsync/public")
	}
	if public.Comment != "Public files, read-only, no auth" {
		t.Errorf("public.Comment = %q, want %q", public.Comment, "Public files, read-only, no auth")
	}
	if !public.ReadOnly {
		t.Errorf("public.ReadOnly = false, want true (inherited global default)")
	}
	if !public.List {
		t.Errorf("public.List = false, want true (inherited global default)")
	}
	wantExclude := []string{"*.tmp", "*.bak", ".git/"}
	if len(public.Exclude) != len(wantExclude) {
		t.Fatalf("public.Exclude = %v, want %v", public.Exclude, wantExclude)
	}
	for i, p := range wantExclude {
		if public.Exclude[i] != p {
			t.Errorf("public.Exclude[%d] = %q, want %q", i, public.Exclude[i], p)
		}
	}

	private, ok := cfg.Modules["private"]
	if !ok {
		t.Fatalf("missing module %q", "private")
	}
	if private.ReadOnly {
		t.Errorf("private.ReadOnly = true, want false (explicit override)")
	}
	if private.List {
		t.Errorf("private.List = true, want false (explicit override)")
	}
	wantUsers := []string{"alice", "bob"}
	if len(private.AuthUsers) != len(wantUsers) {
		t.Fatalf("private.AuthUsers = %v, want %v", private.AuthUsers, wantUsers)
	}
	for i, u := range wantUsers {
		if private.AuthUsers[i] != u {
			t.Errorf("private.AuthUsers[%d] = %q, want %q", i, private.AuthUsers[i], u)
		}
	}
	if private.SecretsFile != "/etc/rsyncd.secrets" {
		t.Errorf("private.SecretsFile = %q, want %q", private.SecretsFile, "/etc/rsyncd.secrets")
	}
	if private.MaxConnections != 4 {
		t.Errorf("private.MaxConnections = %d, want 4", private.MaxConnections)
	}

	backup, ok := cfg.Modules["backup"]
	if !ok {
		t.Fatalf("missing module %q", "backup")
	}
	if !strings.Contains(backup.Path, "continued path is not realistic") {
		t.Errorf("backup.Path = %q, want it to include the continuation line's content", backup.Path)
	}
}

func TestParseConfig_UnrecognizedParameterIsIgnoredNotAnError(t *testing.T) {
	const sample = `
[mod]
path = /srv/data
uid = nobody
gid = nogroup
timeout = 300
`
	cfg, err := ParseConfig(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("ParseConfig with unrecognized-but-valid parameters returned error: %v", err)
	}
	if cfg.Modules["mod"].Path != "/srv/data" {
		t.Errorf("Path = %q, want %q", cfg.Modules["mod"].Path, "/srv/data")
	}
}

func TestParseConfig_MalformedSectionHeaderErrors(t *testing.T) {
	_, err := ParseConfig(strings.NewReader("[unterminated\npath = /srv\n"))
	if err == nil {
		t.Fatalf("ParseConfig with a malformed section header returned nil error, want an error")
	}
}

func TestParseConfig_MalformedMaxConnectionsErrors(t *testing.T) {
	_, err := ParseConfig(strings.NewReader("[mod]\npath = /srv\nmax connections = not-a-number\n"))
	if err == nil {
		t.Fatalf("ParseConfig with a non-numeric max connections returned nil error, want an error")
	}
}

func TestParseConfig_DuplicateModuleErrors(t *testing.T) {
	_, err := ParseConfig(strings.NewReader("[mod]\npath = /a\n[mod]\npath = /b\n"))
	if err == nil {
		t.Fatalf("ParseConfig with a duplicate module name returned nil error, want an error")
	}
}

func TestParseConfig_MissingPathErrors(t *testing.T) {
	_, err := ParseConfig(strings.NewReader("[mod]\nread only = yes\n"))
	if err == nil {
		t.Fatalf("ParseConfig with a module missing \"path\" returned nil error, want an error")
	}
}

func TestParseConfig_MalformedParamLineErrors(t *testing.T) {
	_, err := ParseConfig(strings.NewReader("[mod]\npath = /srv\nthis line has no equals sign\n"))
	if err == nil {
		t.Fatalf("ParseConfig with a malformed parameter line returned nil error, want an error")
	}
}

func TestParseConfig_InvalidBooleanErrors(t *testing.T) {
	_, err := ParseConfig(strings.NewReader("[mod]\npath = /srv\nread only = maybe\n"))
	if err == nil {
		t.Fatalf("ParseConfig with an invalid boolean value returned nil error, want an error")
	}
}
