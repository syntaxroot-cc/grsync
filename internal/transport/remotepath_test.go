package transport

import "testing"

func TestParseRemotePath(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   RemotePath
		wantOK bool
	}{
		{"user host path", "alice@example.com:/home/alice/data", RemotePath{User: "alice", Host: "example.com", Path: "/home/alice/data"}, true},
		{"host path no user", "example.com:/home/alice/data", RemotePath{Host: "example.com", Path: "/home/alice/data"}, true},
		{"relative path after colon", "example.com:data", RemotePath{Host: "example.com", Path: "data"}, true},
		{"ipv6 with user", "alice@[::1]:/data", RemotePath{User: "alice", Host: "::1", Path: "/data"}, true},
		{"ipv6 without user", "[2001:db8::1]:/data", RemotePath{Host: "2001:db8::1", Path: "/data"}, true},

		{"plain relative local path", "src/file.txt", RemotePath{}, false},
		{"plain absolute posix path", "/home/alice/data", RemotePath{}, false},
		{"windows absolute path", `C:\Users\PC\file.txt`, RemotePath{}, false},
		{"windows forward-slash path", "C:/Users/PC/file.txt", RemotePath{}, false},
		{"bare drive letter", "C:", RemotePath{}, false},
		{"lowercase drive letter", `d:\data`, RemotePath{}, false},
		{"slash before colon disqualifies host", "some/dir:with:colons", RemotePath{}, false},
		{"at-sign in local path with slash before it disqualifies user", "some/dir@literal:path", RemotePath{}, false},
		{"empty string", "", RemotePath{}, false},
		{"malformed ipv6 missing close bracket", "[::1:path", RemotePath{}, false},
		{"malformed ipv6 no colon after bracket", "[::1]path", RemotePath{}, false},
		{"rsync daemon URL is not SSH syntax", "rsync://example.com/module", RemotePath{}, false},
		{"rsync daemon URL with user and port is not SSH syntax", "rsync://alice@example.com:8730/module", RemotePath{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseRemotePath(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("ParseRemotePath(%q) ok = %v, want %v (got %+v)", tt.input, ok, tt.wantOK, got)
			}
			if !ok {
				return
			}
			if got != tt.want {
				t.Errorf("ParseRemotePath(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}
