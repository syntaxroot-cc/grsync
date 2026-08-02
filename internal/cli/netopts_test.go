package cli

import (
	"net"
	"testing"
)

func TestTCPNetwork(t *testing.T) {
	tests := []struct {
		name       string
		ipv4, ipv6 bool
		want       string
		wantErr    bool
	}{
		{"neither", false, false, "tcp", false},
		{"ipv4 only", true, false, "tcp4", false},
		{"ipv6 only", false, true, "tcp6", false},
		{"both is an error", true, true, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tcpNetwork(tt.ipv4, tt.ipv6)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("tcpNetwork(%v, %v) returned nil error, want an error for the mutually-exclusive combination", tt.ipv4, tt.ipv6)
				}
				return
			}
			if err != nil {
				t.Fatalf("tcpNetwork(%v, %v) returned error: %v", tt.ipv4, tt.ipv6, err)
			}
			if got != tt.want {
				t.Errorf("tcpNetwork(%v, %v) = %q, want %q", tt.ipv4, tt.ipv6, got, tt.want)
			}
		})
	}
}

func TestResolveLocalAddr_EmptyIsNilNil(t *testing.T) {
	addr, err := resolveLocalAddr("tcp", "")
	if err != nil {
		t.Fatalf("resolveLocalAddr(\"tcp\", \"\") returned error: %v", err)
	}
	if addr != nil {
		t.Errorf("resolveLocalAddr(\"tcp\", \"\") = %v, want nil (no local address preference)", addr)
	}
}

func TestResolveLocalAddr_LiteralIPv4(t *testing.T) {
	addr, err := resolveLocalAddr("tcp4", "127.0.0.1")
	if err != nil {
		t.Fatalf("resolveLocalAddr returned error: %v", err)
	}
	if addr == nil || !addr.IP.Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("resolveLocalAddr(\"tcp4\", \"127.0.0.1\") = %v, want IP 127.0.0.1", addr)
	}
}

func TestResolveLocalAddr_LiteralIPv6(t *testing.T) {
	addr, err := resolveLocalAddr("tcp6", "::1")
	if err != nil {
		t.Fatalf("resolveLocalAddr returned error: %v", err)
	}
	if addr == nil || !addr.IP.Equal(net.ParseIP("::1")) {
		t.Errorf("resolveLocalAddr(\"tcp6\", \"::1\") = %v, want IP ::1", addr)
	}
}

func TestResolveLocalAddr_InvalidAddressReturnsError(t *testing.T) {
	if _, err := resolveLocalAddr("tcp", "this is not a valid address or hostname!!"); err == nil {
		t.Error("resolveLocalAddr with a garbage address returned nil error, want an error")
	}
}

// TestResolveLocalAddr_HonorsNetworkHint confirms --address combined
// with --ipv6 resolves "localhost" to its IPv6 loopback address
// specifically, not whichever family a plain lookup happens to return
// first - matching resolveLocalAddr's own doc comment. Skips gracefully
// if this environment's resolver doesn't map "localhost" to ::1 at all.
func TestResolveLocalAddr_HonorsNetworkHint(t *testing.T) {
	addr, err := resolveLocalAddr("tcp6", "localhost")
	if err != nil {
		t.Skipf("this environment cannot resolve \"localhost\" to an IPv6 address: %v", err)
	}
	if addr.IP.To4() != nil {
		t.Errorf("resolveLocalAddr(\"tcp6\", \"localhost\") = %v, want an IPv6 address, not an IPv4-mapped one", addr)
	}
}
