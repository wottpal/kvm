package refresh

import (
	"math"
	"testing"
)

func TestValidate(t *testing.T) {
	rect := Rect{X: 16, Y: 32, W: 880, H: 224}
	sixRects := []Rect{rect, rect, rect, rect, rect, rect}
	tests := []struct {
		name string
		req  Request
		ok   bool
	}{
		{"full refresh", Request{QP: 8, Frames: DefaultFrames}, true},
		{"one rect", Request{Rects: []Rect{rect}, QP: 8, Frames: 1}, true},
		{"six rects", Request{Rects: sixRects, QP: 51, Frames: MaxFrames}, true},
		{"seven rects", Request{Rects: append(sixRects, rect), QP: 8, Frames: 2}, false},
		{"qp 0", Request{Rects: []Rect{rect}, QP: 0, Frames: 2}, false},
		{"qp 52", Request{QP: 52, Frames: 2}, false},
		{"frames 0", Request{QP: 8, Frames: 0}, false},
		{"frames 5", Request{QP: 8, Frames: 5}, false},
		{"origin, one pixel", Request{Rects: []Rect{{W: 1, H: 1}}, QP: 8, Frames: 2}, true},
		{"negative x", Request{Rects: []Rect{{X: -1, W: 16, H: 16}}, QP: 8, Frames: 2}, false},
		{"negative y", Request{Rects: []Rect{{Y: -1, W: 16, H: 16}}, QP: 8, Frames: 2}, false},
		{"zero width", Request{Rects: []Rect{{W: 0, H: 16}}, QP: 8, Frames: 2}, false},
		{"zero height", Request{Rects: []Rect{{W: 16, H: 0}}, QP: 8, Frames: 2}, false},
		{"ends at the bound", Request{Rects: []Rect{{X: 4096, Y: 4096, W: 4096, H: 4096}}, QP: 8, Frames: 2}, true},
		{"ends past the bound", Request{Rects: []Rect{{X: 4096, W: 4097, H: 16}}, QP: 8, Frames: 2}, false},
		{"sum would overflow", Request{Rects: []Rect{{X: math.MaxInt, W: 1, H: 16}}, QP: 8, Frames: 2}, false},
		{"one bad rect of two", Request{Rects: []Rect{rect, {X: 0, Y: 0, W: 16, H: -16}}, QP: 8, Frames: 2}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.req.Validate(); (err == nil) != tt.ok {
				t.Fatalf("Validate() = %v, want ok %v", err, tt.ok)
			}
		})
	}
}
