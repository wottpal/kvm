//go:build linux && arm && cgo

package audio

/*
#cgo CFLAGS: -I${SRCDIR}/../native/cgo/include/opus
#cgo LDFLAGS: ${SRCDIR}/../native/cgo/lib/libopus.a -lm

#include <opus.h>

// opus_encoder_ctl is variadic; one wrapper sets everything (D14).
static int jk_opus_configure(OpusEncoder *enc, int bitrate, int complexity) {
	int err = opus_encoder_ctl(enc, OPUS_SET_BITRATE(bitrate));
	if (err == OPUS_OK) err = opus_encoder_ctl(enc, OPUS_SET_VBR(1));
	if (err == OPUS_OK) err = opus_encoder_ctl(enc, OPUS_SET_COMPLEXITY(complexity));
	if (err == OPUS_OK) err = opus_encoder_ctl(enc, OPUS_SET_SIGNAL(OPUS_SIGNAL_MUSIC));
	return err;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// OpusEncoder encodes 48 kHz stereo 16-bit PCM frames (D14). Restricted low
// delay: CELT only, 5 ms of look-ahead instead of 26.5, and a remote
// computer's sound is mostly music and system sounds rather than speech.
type OpusEncoder struct {
	enc *C.OpusEncoder
	out []byte
}

const (
	opusBitrate    = 128000
	opusComplexity = 3
	opusMaxPacket  = 1500
)

func NewOpusEncoder() (*OpusEncoder, error) {
	var err C.int
	enc := C.opus_encoder_create(C.opus_int32(CaptureSampleRate), C.int(CaptureChannels), C.OPUS_APPLICATION_RESTRICTED_LOWDELAY, &err)
	if err != C.OPUS_OK || enc == nil {
		return nil, fmt.Errorf("opus_encoder_create: %d", int(err))
	}
	if rc := C.jk_opus_configure(enc, C.int(opusBitrate), C.int(opusComplexity)); rc != C.OPUS_OK {
		C.opus_encoder_destroy(enc)
		return nil, fmt.Errorf("opus_encoder_ctl: %d", int(rc))
	}
	return &OpusEncoder{enc: enc, out: make([]byte, opusMaxPacket)}, nil
}

// Encode encodes one CaptureFrameSize frame of interleaved stereo samples.
// The returned slice is reused by the next call.
func (e *OpusEncoder) Encode(pcm []int16) ([]byte, error) {
	n := C.opus_encode(e.enc, (*C.opus_int16)(unsafe.Pointer(&pcm[0])), C.int(CaptureFrameSize),
		(*C.uchar)(unsafe.Pointer(&e.out[0])), C.opus_int32(len(e.out)))
	if n < 0 {
		return nil, fmt.Errorf("opus_encode: %d", int(n))
	}
	return e.out[:n], nil
}

func (e *OpusEncoder) Close() {
	if e.enc != nil {
		C.opus_encoder_destroy(e.enc)
		e.enc = nil
	}
}
