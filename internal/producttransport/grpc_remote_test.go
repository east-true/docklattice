package producttransport

import (
	"net"
	"testing"
)

func TestRemoteIPAcceptsOnlyCanonicalPeerIP(t *testing.T) {
	tests := []struct {
		addr net.Addr
		want string
	}{
		{addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.9"), Port: 9443}, want: "192.0.2.9"},
		{addr: &net.TCPAddr{IP: net.ParseIP("2001:db8::5"), Port: 9443}, want: "2001:db8::5"},
		{addr: stringAddr("unix:///run/docklattice.sock"), want: ""},
		{addr: nil, want: ""},
	}
	for _, test := range tests {
		if got := remoteIP(test.addr); got != test.want {
			t.Fatalf("remoteIP(%v) = %q, want %q", test.addr, got, test.want)
		}
	}
}

type stringAddr string

func (a stringAddr) Network() string { return "test" }
func (a stringAddr) String() string  { return string(a) }
