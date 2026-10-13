package uvk5

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeSysfs builds /sys/class/tty/<tty>/device -> .../usb1/1-1/1-1:1.0
// with the device's ids one level up, and a /dev node for it.
func fakeSysfs(t *testing.T, tty, vendor, product string) string {
	t.Helper()
	root := t.TempDir()
	usbDev := filepath.Join(root, "devices", "usb1", "1-1")
	iface := filepath.Join(usbDev, "1-1:1.0")
	if err := os.MkdirAll(iface, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(usbDev, "idVendor"), []byte(vendor+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(usbDev, "idProduct"), []byte(product+"\n"), 0o644)
	cls := filepath.Join(root, "class", "tty", tty)
	_ = os.MkdirAll(cls, 0o755)
	if err := os.Symlink(iface, filepath.Join(cls, "device")); err != nil {
		t.Fatal(err)
	}
	old := sysfsTTY
	sysfsTTY = filepath.Join(root, "class", "tty")
	t.Cleanup(func() { sysfsTTY = old })
	dev := filepath.Join(root, "dev", tty)
	_ = os.MkdirAll(filepath.Dir(dev), 0o755)
	_ = os.WriteFile(dev, nil, 0o644)
	return dev
}

func TestSafeForDTR(t *testing.T) {
	radio := fakeSysfs(t, "ttyACM0", "36B7", "ffff")
	if id, err := PortUSBID(radio); err != nil || id.String() != "36b7:ffff" {
		t.Fatalf("radio id %v %v", id, err)
	}
	if err := SafeForDTR(radio); err != nil {
		t.Fatalf("UV-K5 refused: %v", err)
	}
	aioc := fakeSysfs(t, "ttyACM1", "1209", "7388")
	if err := SafeForDTR(aioc); err == nil {
		t.Fatal("AIOC accepted")
	}
	if err := SafeForDTR(filepath.Join(t.TempDir(), "nothing")); err == nil {
		t.Fatal("unknown port accepted")
	}
}
