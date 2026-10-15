//go:build !linux

package hostlink

import (
	"errors"

	"github.com/rs/zerolog"
)

// Open needs Linux's packet sockets.
func Open(ifname string, log *zerolog.Logger) (*Link, error) {
	return nil, errors.New("the host link needs Linux")
}
