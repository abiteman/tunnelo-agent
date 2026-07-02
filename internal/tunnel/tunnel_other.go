//go:build !linux

package tunnel

import (
	"errors"
	"log/slog"
)

// The agent targets Linux hosts (Docker, bare metal, Unraid). Other
// platforms compile but cannot bring a tunnel up.
func openDevice(Config, *slog.Logger) (wgDevice, string, error) {
	return nil, "", errors.New("tunnel management is only supported on Linux")
}
