//go:build !linux

package uvk5

import (
	"errors"
	"io"
)

// OpenPort is only implemented on Linux (the nodes).
func OpenPort(path string) (io.ReadWriteCloser, error) {
	return nil, errors.New("uvk5: serial ports are only supported on Linux")
}
