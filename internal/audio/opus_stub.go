//go:build !linux || !arm || !cgo

package audio

import "errors"

// OpusEncoder needs libopus, which the native build compiles for the device
// only (D14); other builds (CI's host tests and lint) get this stub.
type OpusEncoder struct{}

var errOpusUnavailable = errors.New("opus encoding is not available for this build")

func NewOpusEncoder() (*OpusEncoder, error) { return nil, errOpusUnavailable }

func (*OpusEncoder) Encode([]int16) ([]byte, error) { return nil, errOpusUnavailable }

func (*OpusEncoder) Close() {}
