//go:build linux

package uvk5

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A pseudo-terminal stands in for the radio's USB serial port: the
// session runs on OpenPort's end, the fake radio on the other.
func TestOpenPortWithPTY(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no /dev/ptmx:", err)
	}
	defer master.Close()
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	port, err := OpenPort(fmt.Sprintf("/dev/pts/%d", n))
	if err != nil {
		t.Fatal(err)
	}
	radio := NewFakeRadio("KD2DCM-4", time.Now)
	go func() { _ = radio.Serve(master) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Connect(ctx, port)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, "KD2DCM-1", "over a real tty"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close didn't end the session")
	}
}
