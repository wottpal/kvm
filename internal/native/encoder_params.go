package native

import (
	"fmt"

	pb "github.com/jetkvm/kvm/internal/native/proto"
)

// VideoEncoderParams are the runtime-tunable VENC settings (cgo/video.c).
// Defaults come from JETKVM_VENC_* env or compiled values. Changes are kept in
// memory only and reset to the defaults when the last session disconnects and
// on setStreamQualityFactor.
type VideoEncoderParams struct {
	MinQP      int     `json:"minQp"`
	MaxQP      int     `json:"maxQp"`
	GOPSeconds float64 `json:"gopSeconds"`
	AQ         bool    `json:"aq"`
	MaxFPS     int     `json:"maxFps"` // 0 = encode at the source rate
	// Explicit RC targets; 0 = derived from the stream quality factor.
	BitrateKbps    int `json:"bitrateKbps"`
	MaxBitrateKbps int `json:"maxBitrateKbps"`
	// QP of the keyframe that refreshes a still screen (1-51; minQp or more
	// turns the refresh off).
	StillQP int `json:"stillQp"`
	// AutoRefresh lets the device start still refreshes on its own; a client
	// that drives them (refreshRegions, D13) turns it off.
	AutoRefresh bool `json:"autoRefresh"`
}

// VideoEncoderParamsUpdate is a partial VideoEncoderParams; nil fields keep
// their current value.
type VideoEncoderParamsUpdate struct {
	MinQP      *int     `json:"minQp"`
	MaxQP      *int     `json:"maxQp"`
	GOPSeconds *float64 `json:"gopSeconds"`
	AQ         *bool    `json:"aq"`
	MaxFPS     *int     `json:"maxFps"`

	BitrateKbps    *int  `json:"bitrateKbps"`
	MaxBitrateKbps *int  `json:"maxBitrateKbps"`
	StillQP        *int  `json:"stillQp"`
	AutoRefresh    *bool `json:"autoRefresh"`
}

// VideoEncoderStatus is the effective encoder configuration.
// CaptureMaxBytes is the size of each capture buffer (VIDEO_CAPTURE_MAX_BYTES in
// cgo/video.c): a mode needs width x height x 2 bytes at most. Upstream's buffers
// hold 1920x1080x3 bytes.
const CaptureMaxBytes = 8 << 20

type VideoEncoderStatus struct {
	VideoEncoderParams
	// CaptureMaxBytes tells clients the largest mode the device captures.
	CaptureMaxBytes         int    `json:"captureMaxBytes"`
	SourceFPS               int    `json:"sourceFps"`
	EncodeFPS               int    `json:"encodeFps"`
	GOPFrames               int    `json:"gopFrames"`
	EffectiveBitrateKbps    int    `json:"effectiveBitrateKbps"`
	EffectiveMaxBitrateKbps int    `json:"effectiveMaxBitrateKbps"`
	Codec                   string `json:"codec"`
	Streaming               bool   `json:"streaming"`
}

// Validate mirrors venc_params_valid in cgo/video.c.
func (p VideoEncoderParams) Validate() error {
	switch {
	case p.MinQP < 8 || p.MinQP > 40:
		return fmt.Errorf("minQp must be 8-40, got %d", p.MinQP)
	case p.MaxQP < p.MinQP || p.MaxQP > 51:
		return fmt.Errorf("maxQp must be %d-51 (minQp-51), got %d", p.MinQP, p.MaxQP)
	case !(p.GOPSeconds >= 0.5 && p.GOPSeconds <= 300):
		return fmt.Errorf("gopSeconds must be 0.5-300, got %g", p.GOPSeconds)
	case p.MaxFPS < 0 || p.MaxFPS > 120:
		return fmt.Errorf("maxFps must be 1-120, or 0 for the source rate, got %d", p.MaxFPS)
	case p.BitrateKbps != 0 && (p.BitrateKbps < 100 || p.BitrateKbps > 50000):
		return fmt.Errorf("bitrateKbps must be 100-50000, or 0 for the quality factor, got %d", p.BitrateKbps)
	case p.MaxBitrateKbps != 0 && (p.MaxBitrateKbps < 100 || p.MaxBitrateKbps > 50000):
		return fmt.Errorf("maxBitrateKbps must be 100-50000, or 0 for 1.5x the target, got %d", p.MaxBitrateKbps)
	case p.BitrateKbps != 0 && p.MaxBitrateKbps != 0 && p.MaxBitrateKbps < p.BitrateKbps:
		return fmt.Errorf("maxBitrateKbps (%d) must be >= bitrateKbps (%d)", p.MaxBitrateKbps, p.BitrateKbps)
	case p.StillQP < 1 || p.StillQP > 51:
		return fmt.Errorf("stillQp must be 1-51, got %d", p.StillQP)
	}
	return nil
}

// ApplyTo returns p with every field set in u replaced.
func (u VideoEncoderParamsUpdate) ApplyTo(p VideoEncoderParams) VideoEncoderParams {
	if u.MinQP != nil {
		p.MinQP = *u.MinQP
	}
	if u.MaxQP != nil {
		p.MaxQP = *u.MaxQP
	}
	if u.GOPSeconds != nil {
		p.GOPSeconds = *u.GOPSeconds
	}
	if u.AQ != nil {
		p.AQ = *u.AQ
	}
	if u.MaxFPS != nil {
		p.MaxFPS = *u.MaxFPS
	}
	if u.BitrateKbps != nil {
		p.BitrateKbps = *u.BitrateKbps
	}
	if u.MaxBitrateKbps != nil {
		p.MaxBitrateKbps = *u.MaxBitrateKbps
	}
	if u.StillQP != nil {
		p.StillQP = *u.StillQP
	}
	if u.AutoRefresh != nil {
		p.AutoRefresh = *u.AutoRefresh
	}
	return p
}

func videoCodecName(codecType int) string {
	if codecType == 1 {
		return "h265"
	}
	return "h264"
}

func (p VideoEncoderParams) toProto() *pb.VideoEncoderParams {
	return &pb.VideoEncoderParams{
		MinQp:          int32(p.MinQP),
		MaxQp:          int32(p.MaxQP),
		GopSeconds:     p.GOPSeconds,
		Aq:             p.AQ,
		MaxFps:         int32(p.MaxFPS),
		BitrateKbps:    int32(p.BitrateKbps),
		MaxBitrateKbps: int32(p.MaxBitrateKbps),
		StillQp:        int32(p.StillQP),
		AutoRefresh:    p.AutoRefresh,
	}
}

func videoEncoderParamsFromProto(p *pb.VideoEncoderParams) VideoEncoderParams {
	return VideoEncoderParams{
		MinQP:          int(p.GetMinQp()),
		MaxQP:          int(p.GetMaxQp()),
		GOPSeconds:     p.GetGopSeconds(),
		AQ:             p.GetAq(),
		MaxFPS:         int(p.GetMaxFps()),
		BitrateKbps:    int(p.GetBitrateKbps()),
		MaxBitrateKbps: int(p.GetMaxBitrateKbps()),
		StillQP:        int(p.GetStillQp()),
		AutoRefresh:    p.GetAutoRefresh(),
	}
}

func (s VideoEncoderStatus) toProto() *pb.VideoEncoderStatus {
	return &pb.VideoEncoderStatus{
		Params:                  s.VideoEncoderParams.toProto(),
		SourceFps:               int32(s.SourceFPS),
		EncodeFps:               int32(s.EncodeFPS),
		GopFrames:               int32(s.GOPFrames),
		EffectiveBitrateKbps:    int32(s.EffectiveBitrateKbps),
		EffectiveMaxBitrateKbps: int32(s.EffectiveMaxBitrateKbps),
		Codec:                   s.Codec,
		Streaming:               s.Streaming,
	}
}

func videoEncoderStatusFromProto(s *pb.VideoEncoderStatus) VideoEncoderStatus {
	return VideoEncoderStatus{
		VideoEncoderParams:      videoEncoderParamsFromProto(s.GetParams()),
		SourceFPS:               int(s.GetSourceFps()),
		EncodeFPS:               int(s.GetEncodeFps()),
		GOPFrames:               int(s.GetGopFrames()),
		EffectiveBitrateKbps:    int(s.GetEffectiveBitrateKbps()),
		EffectiveMaxBitrateKbps: int(s.GetEffectiveMaxBitrateKbps()),
		Codec:                   s.GetCodec(),
		Streaming:               s.GetStreaming(),
		CaptureMaxBytes:         CaptureMaxBytes,
	}
}
