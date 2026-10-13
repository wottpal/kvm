//go:build linux && cgo

package audio

import (
	"math"
	"testing"
)

func TestOpusEncoderFrame(t *testing.T) {
	encoder, err := NewOpusEncoder()
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()

	pcm := make([]int16, CaptureFrameSize*CaptureChannels)
	for i := 0; i < CaptureFrameSize; i++ {
		v := int16(math.Sin(float64(i)*2*math.Pi*440/CaptureSampleRate) * 12000)
		pcm[2*i], pcm[2*i+1] = v, v
	}
	for range 5 {
		packet, err := encoder.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		// 128 kbit/s VBR is about 320 bytes per 20 ms frame.
		if len(packet) < 50 || len(packet) > opusMaxPacket {
			t.Fatalf("packet size = %d bytes", len(packet))
		}
	}
}
