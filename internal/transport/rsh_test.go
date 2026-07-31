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
	got := BuildRSHCommand("", "alice", "example.com", []string{"grsync", "--server"})
	want := []string{"ssh", "alice@example.com", "grsync", "--server"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

func TestBuildRSHCommand_NoUser(t *testing.T) {
	got := BuildRSHCommand("", "", "example.com", []string{"grsync", "--server"})
	want := []string{"ssh", "example.com", "grsync", "--server"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

func TestBuildRSHCommand_CustomRSH(t *testing.T) {
	got := BuildRSHCommand("ssh -p 2222 -i key.pem", "alice", "example.com", []string{"grsync", "--server"})
	want := []string{"ssh", "-p", "2222", "-i", "key.pem", "alice@example.com", "grsync", "--server"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("BuildRSHCommand = %v, want %v", got, want)
	}
}

func TestBuildRSHCommand_QuotedArgumentSurvivesAsOneField(t *testing.T) {
	got := BuildRSHCommand(`ssh -o "ProxyCommand=nc %h %p"`, "", "example.com", nil)
	want := []string{"ssh", "-o", "ProxyCommand=nc %h %p", "example.com"}
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
