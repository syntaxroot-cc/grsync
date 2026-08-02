package transport

import "testing"

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBuildRSHCommand_Default(t *testing.T) {
	got := BuildRSHCommand("", "alice", "example.com", []string{"grsync", "--server"}, false, false)
	want := []string{"ssh", "alice@example.com", "grsync", "--server"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

func TestBuildRSHCommand_NoUser(t *testing.T) {
	got := BuildRSHCommand("", "", "example.com", []string{"grsync", "--server"}, false, false)
	want := []string{"ssh", "example.com", "grsync", "--server"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

func TestBuildRSHCommand_CustomRSH(t *testing.T) {
	got := BuildRSHCommand("ssh -p 2222 -i key.pem", "alice", "example.com", []string{"grsync", "--server"}, false, false)
	want := []string{"ssh", "-p", "2222", "-i", "key.pem", "alice@example.com", "grsync", "--server"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

func TestBuildRSHCommand_QuotedArgumentSurvivesAsOneField(t *testing.T) {
	got := BuildRSHCommand(`ssh -o "ProxyCommand=nc %h %p"`, "", "example.com", nil, false, false)
	want := []string{"ssh", "-o", "ProxyCommand=nc %h %p", "example.com"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

// TestBuildRSHCommand_IPv4ForwardedToDefaultSSH is SC-14's core proof:
// with no --rsh override at all (the default "ssh" is used), --ipv4
// must insert a real "-4" into the ssh argv, right before the target -
// matching real rsync's own exact insertion point (main.c's do_cmd(),
// verified against upstream source: args[argc++] = "-4"; happens
// immediately before args[argc++] = machine;).
func TestBuildRSHCommand_IPv4ForwardedToDefaultSSH(t *testing.T) {
	got := BuildRSHCommand("", "", "example.com", []string{"grsync", "--server"}, true, false)
	want := []string{"ssh", "-4", "example.com", "grsync", "--server"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

func TestBuildRSHCommand_IPv6ForwardedToDefaultSSH(t *testing.T) {
	got := BuildRSHCommand("", "", "example.com", []string{"grsync", "--server"}, false, true)
	want := []string{"ssh", "-6", "example.com", "grsync", "--server"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

// TestBuildRSHCommand_IPv4ForwardedWhenRSHOverrideIsStillSSH confirms
// the forwarding survives an --rsh override, as long as the resolved
// program is still genuinely ssh (e.g. "ssh -p 2222 -i key.pem") -
// real rsync's own check is on the resolved program's basename, not on
// whether --rsh/-e was given at all.
func TestBuildRSHCommand_IPv4ForwardedWhenRSHOverrideIsStillSSH(t *testing.T) {
	got := BuildRSHCommand("ssh -p 2222 -i key.pem", "alice", "example.com", []string{"grsync", "--server"}, true, false)
	want := []string{"ssh", "-p", "2222", "-i", "key.pem", "-4", "alice@example.com", "grsync", "--server"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

// TestBuildRSHCommand_NotForwardedForNonSSHRemoteShell is real rsync's
// own documented limit made concrete: "For other remote shells you'll
// need to specify '--rsh SHELL -4' directly" - so --ipv4/--ipv6 must be
// silently NOT forwarded when the remote shell isn't ssh, rather than
// guessing at some other program's own address-family flag syntax.
func TestBuildRSHCommand_NotForwardedForNonSSHRemoteShell(t *testing.T) {
	got := BuildRSHCommand("rsh", "", "example.com", []string{"grsync", "--server"}, true, false)
	want := []string{"rsh", "example.com", "grsync", "--server"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

// TestBuildRSHCommand_IPv4ForwardedForFullSSHPath confirms the basename
// check, not an exact-string check: an --rsh override giving ssh's full
// path must still be recognized as ssh.
func TestBuildRSHCommand_IPv4ForwardedForFullSSHPath(t *testing.T) {
	got := BuildRSHCommand("/usr/bin/ssh", "", "example.com", nil, true, false)
	want := []string{"/usr/bin/ssh", "-4", "example.com"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

// TestBuildRSHCommand_IPv4ForwardedForWindowsSSHExe confirms the ".exe"
// stripping grsync's own cross-platform (native Windows) support needs
// that real rsync's Unix-only C code never had to handle.
func TestBuildRSHCommand_IPv4ForwardedForWindowsSSHExe(t *testing.T) {
	got := BuildRSHCommand(`C:\Windows\System32\OpenSSH\ssh.exe`, "", "example.com", nil, true, false)
	want := []string{`C:\Windows\System32\OpenSSH\ssh.exe`, "-4", "example.com"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

func TestBuildRSHCommand_NeitherIPv4NorIPv6RequestedInsertsNothing(t *testing.T) {
	got := BuildRSHCommand("", "", "example.com", []string{"grsync", "--server"}, false, false)
	want := []string{"ssh", "example.com", "grsync", "--server"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

func TestSplitRSHCommand(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty", "", nil},
		{"single word", "ssh", []string{"ssh"}},
		{"multiple words", "ssh -p 2222", []string{"ssh", "-p", "2222"}},
		{"double quoted argument", `ssh -o "a b c"`, []string{"ssh", "-o", "a b c"}},
		{"single quoted argument", `ssh -o 'a b c'`, []string{"ssh", "-o", "a b c"}},
		{"extra whitespace collapses", "ssh   -p   2222", []string{"ssh", "-p", "2222"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitRSHCommand(tt.input)
			if !stringSlicesEqual(got, tt.want) {
				t.Errorf("splitRSHCommand(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
