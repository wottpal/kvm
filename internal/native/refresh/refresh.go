// Package refresh describes a client-driven refresh (FORK.md D13): rects the
// encoder re-encodes as forced-intra blocks at an absolute QP for a few
// frames, or, without rects, the whole frame as a keyframe. It is plain Go, so
// it builds and tests on any host (the native package needs the device's cgo
// libraries); video_refresh_regions in internal/native/cgo/video.c mirrors it.
package refresh

import "fmt"

const (
	// MaxRects is the number of encoder ROIs a request may use (indexes 0-5;
	// the still refresh owns index 7).
	MaxRects = 6
	// DefaultFrames is the frame count of a request that names none.
	DefaultFrames = 2
	MaxFrames     = 4
	// maxCoord bounds the rects (no mode is that large; the encoder clips
	// them to the frame) and keeps their sums from overflowing.
	maxCoord = 8192
)

// Rect is a region in frame pixels. The encoder aligns it outward to its
// 16-pixel blocks and clips it to the frame; one entirely outside the frame
// is an error.
type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Request refreshes Rects at QP for the next Frames encoded frames, or the
// whole frame with a keyframe at QP when Rects is empty.
type Request struct {
	Rects  []Rect
	QP     int
	Frames int
}

func (r Request) Validate() error {
	switch {
	case len(r.Rects) > MaxRects:
		return fmt.Errorf("at most %d rects, got %d", MaxRects, len(r.Rects))
	case r.QP < 1 || r.QP > 51:
		return fmt.Errorf("qp must be 1-51, got %d", r.QP)
	case r.Frames < 1 || r.Frames > MaxFrames:
		return fmt.Errorf("frames must be 1-%d, got %d", MaxFrames, r.Frames)
	}
	for i, rect := range r.Rects {
		if rect.X < 0 || rect.Y < 0 || rect.W < 1 || rect.H < 1 || rect.W > maxCoord-rect.X || rect.H > maxCoord-rect.Y {
			return fmt.Errorf("rect %d (x %d, y %d, w %d, h %d) needs x, y >= 0, w, h >= 1 and must end within %d",
				i, rect.X, rect.Y, rect.W, rect.H, maxCoord)
		}
	}
	return nil
}
