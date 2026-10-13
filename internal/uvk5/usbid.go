package uvk5

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sysfsTTY is where Linux describes tty devices (a variable for tests).
var sysfsTTY = "/sys/class/tty"

// USBID is a serial port's USB vendor and product id, lower-case hex.
type USBID struct{ Vendor, Product string }

func (u USBID) String() string { return u.Vendor + ":" + u.Product }

// aiocID is the AIOC cable's: it keys the radio's PTT from DTR and RTS.
var aiocID = USBID{"1209", "7388"}

// PortUSBID reads path's USB ids from sysfs (path may be a /dev/serial
// symlink).
func PortUSBID(path string) (USBID, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return USBID{}, err
	}
	dev, err := filepath.EvalSymlinks(filepath.Join(sysfsTTY, filepath.Base(real), "device"))
	if err != nil {
		return USBID{}, fmt.Errorf("uvk5: %s isn't a USB serial port: %w", path, err)
	}
	// device is the USB interface; the ids are on its parent, the device.
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(dev), name))
		return strings.ToLower(strings.TrimSpace(string(b))), err
	}
	v, err := read("idVendor")
	if err != nil {
		return USBID{}, fmt.Errorf("uvk5: %s: no USB ids: %w", path, err)
	}
	p, err := read("idProduct")
	if err != nil {
		return USBID{}, fmt.Errorf("uvk5: %s: no USB ids: %w", path, err)
	}
	return USBID{v, p}, nil
}

// SafeForDTR refuses a port that could key a radio from DTR (an AIOC),
// or whose USB ids can't be read.
func SafeForDTR(path string) error {
	id, err := PortUSBID(path)
	if err != nil {
		return err
	}
	if id == aiocID {
		return fmt.Errorf("uvk5: %s is an AIOC cable (%s), not a UV-K5", path, id)
	}
	return nil
}
