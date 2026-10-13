package audio

import "errors"

const (
	CaptureSampleRate    = 48000
	CaptureChannels      = 2
	CaptureFrameSize     = 960
	PCMUFrameSize        = 160
	G722InputSampleRate  = 16000
	G722InputFrameSize   = 320
	G722EncodedFrameSize = 160
)

type Codec int

const (
	CodecPCMU Codec = iota
	CodecG722
	CodecOpus // 48 kHz stereo (D14)
)

func (c Codec) String() string {
	switch c {
	case CodecG722:
		return "G722"
	case CodecPCMU:
		return "PCMU"
	case CodecOpus:
		return "Opus"
	default:
		return "unknown"
	}
}

// ErrNoAudioData is returned when the capture device produced no frames this
// poll cycle. The caller should retry; it is not a fatal error.
var ErrNoAudioData = errors.New("audio capture idle")

// silenceLevel: a frame whose samples all stay within it is digital silence
// (a host's mixer outputs exact zeros, dither stays within a few LSB).
const silenceLevel = 2

func isSilent(pcm []int16) bool {
	for _, v := range pcm {
		if v > silenceLevel || v < -silenceLevel {
			return false
		}
	}
	return true
}
