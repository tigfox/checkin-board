//go:build linux

package uvk5

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// OpenPort opens the radio's serial port raw (8N1, 38400 baud, which
// USB-C ignores), with DTR and RTS low so a cable that keys PTT from
// them can't key the radio. The port is non-blocking, so Close ends a
// pending Read.
func OpenPort(path string) (io.ReadWriteCloser, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("uvk5: open %s: %w", path, err)
	}
	if err := rawMode(fd); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("uvk5: %s: %w", path, err)
	}
	// Not every adapter has modem lines; USB-C CDC ignores them.
	_ = unix.IoctlSetPointerInt(fd, unix.TIOCMBIC, unix.TIOCM_DTR|unix.TIOCM_RTS)
	return os.NewFile(uintptr(fd), path), nil
}

func rawMode(fd int) error {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL |
		unix.IXON | unix.IXOFF | unix.IXANY
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CRTSCTS | unix.CBAUD
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL | unix.B38400
	t.Ispeed, t.Ospeed = unix.B38400, unix.B38400
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}
