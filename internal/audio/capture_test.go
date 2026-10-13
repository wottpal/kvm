package audio

import "testing"

func TestIsSilent(t *testing.T) {
	frame := make([]int16, CaptureFrameSize*CaptureChannels)
	if !isSilent(frame) {
		t.Fatal("zero frame is not silent")
	}
	frame[100], frame[101] = silenceLevel, -silenceLevel
	if !isSilent(frame) {
		t.Fatal("dither within the silence level is not silent")
	}
	frame[len(frame)-1] = silenceLevel + 1
	if isSilent(frame) {
		t.Fatal("a sample above the silence level counts as silent")
	}
}
