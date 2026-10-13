package firmware

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"checkin-board/internal/uvk5"
)

func TestFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "firmware")
	fo := NewFolder(dir)
	if l, err := fo.List(); err != nil || len(l) != 0 {
		t.Fatalf("empty = %v %v", l, err)
	}
	img := []byte("firmware image")
	if err := fo.Save("f4hwn.v6.1.bin", img, false); err != nil {
		t.Fatal(err)
	}
	if err := fo.Save("f4hwn.v6.1.bin", img, false); !errors.Is(err, ErrExists) {
		t.Fatalf("again: %v", err)
	}
	for _, bad := range []string{"../x.bin", "x.json", ".hidden.bin", "a/b.bin"} {
		if err := fo.Save(bad, img, false); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if err := fo.Save("empty.bin", nil, false); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty image: %v", err)
	}
	if err := fo.Save("big.bin", make([]byte, uvk5.MaxSlotImage+1), false); !errors.Is(err, ErrInvalid) {
		t.Errorf("big image: %v", err)
	}
	// A symlink planted in the folder is never read.
	_ = os.Symlink("/etc/passwd", filepath.Join(dir, "link.bin"))
	l, err := fo.List()
	if err != nil || len(l) != 1 || l[0].Name != "f4hwn.v6.1.bin" || l[0].Size != int64(len(img)) || len(l[0].SHA256) != 64 {
		t.Fatalf("list = %+v %v", l, err)
	}
	if _, err := fo.Read("link.bin"); err == nil {
		t.Fatal("symlink read")
	}
	if got, err := fo.Read("f4hwn.v6.1.bin"); err != nil || string(got) != string(img) {
		t.Fatalf("read = %q %v", got, err)
	}
	if err := fo.Delete("f4hwn.v6.1.bin"); err != nil {
		t.Fatal(err)
	}
	if SafeName(`C:\fw\F4HWN Fusion v6.1.bin`) != "F4HWN-Fusion-v6.1.bin" || SafeName("...") != "" {
		t.Fatalf("SafeName = %q", SafeName(`C:\fw\F4HWN Fusion v6.1.bin`))
	}
}
