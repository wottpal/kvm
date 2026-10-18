//go:build linux && arm

package kvm

import (
	"slices"
	"strings"
	"testing"
)

// A local client may name STUN servers only (D19): no TURN, nothing pion
// can't parse (it would fail the session), a few.
func TestLocalSTUNServers(t *testing.T) {
	got := localSTUNServers([]string{
		"stun:stun.example.com:3478",
		"turn:turn.example.com:3478",
		"stuns:stun.example.org:5349",
		"stun:",
		"stun:host:abc",
		"stun:[::1",
		"stun:host?transport=udp",
		"http://example.com",
		"STUN:stun.example.net",
		"stun:" + strings.Repeat("a", 300),
		"stun:a", "stun:b",
	})
	want := []string{"stun:stun.example.com:3478", "STUN:stun.example.net", "stun:a", "stun:b"}
	if !slices.Equal(got, want) {
		t.Fatalf("localSTUNServers = %q, want %q", got, want)
	}
	if got := localSTUNServers(nil); got != nil {
		t.Fatalf("no servers: %q", got)
	}
}
