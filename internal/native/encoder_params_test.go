//go:build linux && arm

package native

import "testing"

// paceKbps (D18): 0 is off, 100-50000 paces, anything else is refused; an
// update sets it, and it survives the trip through the native process.
func TestPaceKbps(t *testing.T) {
	base := VideoEncoderParams{MinQP: 18, MaxQP: 42, GOPSeconds: 60, StillQP: 8}
	for pace, ok := range map[int]bool{0: true, 100: true, 2000: true, 50000: true, 99: false, -1: false, 50001: false} {
		p := base
		p.PaceKbps = pace
		if err := p.Validate(); (err == nil) != ok {
			t.Errorf("paceKbps %d: %v", pace, err)
		}
	}
	pace := 2000
	p := VideoEncoderParamsUpdate{PaceKbps: &pace}.ApplyTo(base)
	if p.PaceKbps != 2000 || p.MinQP != 18 {
		t.Fatalf("update: %+v", p)
	}
	if back := videoEncoderParamsFromProto(p.toProto()); back != p {
		t.Fatalf("proto round trip: %+v, want %+v", back, p)
	}
}
