package daemon

import "testing"

func TestParseURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  URL
	}{
		{"host and module", "rsync://example.com/mymodule", URL{Host: "example.com", Module: "mymodule"}},
		{"host, module, and path", "rsync://example.com/mymodule/some/dir", URL{Host: "example.com", Module: "mymodule", Path: "some/dir"}},
		{"user and host", "rsync://alice@example.com/mymodule", URL{User: "alice", Host: "example.com", Module: "mymodule"}},
		{"explicit port", "rsync://example.com:8730/mymodule", URL{Host: "example.com", Port: 8730, Module: "mymodule"}},
		{"user, port, module, and path", "rsync://bob@example.com:8730/mod/a/b", URL{User: "bob", Host: "example.com", Port: 8730, Module: "mod", Path: "a/b"}},
		{"no module (list-modules form)", "rsync://example.com", URL{Host: "example.com"}},
		{"no module, trailing slash", "rsync://example.com/", URL{Host: "example.com"}},
		{"ipv6 host", "rsync://[::1]/mymodule", URL{Host: "::1", Module: "mymodule"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseURL(tt.input)
			if err != nil {
				t.Fatalf("ParseURL(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseURL(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseURL_InvalidVariants(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"wrong scheme", "http://example.com/mymodule"},
		{"no scheme at all", "example.com/mymodule"},
		{"ssh-style syntax, not a URL", "user@example.com:mymodule"},
		{"empty host", "rsync:///mymodule"},
		{"empty string", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseURL(tt.input); err == nil {
				t.Errorf("ParseURL(%q) returned nil error, want an error", tt.input)
			}
		})
	}
}
