package uvk5

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func basicSession(t *testing.T, edit func(*FakeRadio)) (*Session, *FakeRadio) {
	t.Helper()
	radio := NewFakeRadio("KD2DCM-4", time.Now)
	if edit != nil {
		edit(radio)
	}
	host, dev := net.Pipe()
	go func() { _ = radio.Serve(dev) }()
	t.Cleanup(func() { _ = dev.Close() })
	s, err := ConnectBasic(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, radio
}

func image(n int) []byte {
	img := make([]byte, n)
	for i := range img {
		img[i] = byte(i*13 + 7)
	}
	return img
}

func TestWriteSlot(t *testing.T) {
	s, radio := basicSession(t, nil)
	ctx := context.Background()
	img := image(3*slotChunk + 50)
	var steps []string
	var last SlotProgress
	err := s.WriteSlot(ctx, 2, img, "Aprs", "v6.1", func(p SlotProgress) {
		if len(steps) == 0 || steps[len(steps)-1] != p.Step {
			steps = append(steps, p.Step)
		}
		last = p
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(steps); got != 3 || steps[0] != "erasing" || steps[2] != "verifying" || last.Done != len(img) || last.Total != len(img) {
		t.Fatalf("steps %v, last %+v", steps, last)
	}
	info, stored := radio.Slot(2)
	if info.Status != SlotValid || info.Name != "Aprs" || info.Version != "v6.1" || !bytes.Equal(stored, img) {
		t.Fatalf("slot = %+v", info)
	}
	// Read back over the wire.
	got, err := s.SlotInfo(ctx, 2)
	if err != nil || got.State != "valid" || got.Size != len(img) || got.Name != "Aprs" {
		t.Fatalf("info = %+v %v", got, err)
	}
	if e, _ := s.SlotInfo(ctx, 3); e.State != "empty" {
		t.Fatalf("empty slot = %+v", e)
	}
	// Writing again replaces it.
	img2 := image(100)
	if err := s.WriteSlot(ctx, 2, img2, "FM Voice", "v1.0", nil); err != nil {
		t.Fatal(err)
	}
	if info, stored := radio.Slot(2); info.Name != "FM Voice" || !bytes.Equal(stored, img2) {
		t.Fatalf("rewritten = %+v", info)
	}
}

func TestWriteSlotRefusals(t *testing.T) {
	s, _ := basicSession(t, nil)
	ctx := context.Background()
	for _, slot := range []int{0, 5, -1} { // 0 is the radio's backup of its main firmware
		if err := s.WriteSlot(ctx, slot, image(10), "x", "", nil); err == nil {
			t.Errorf("slot %d written", slot)
		}
	}
	if err := s.WriteSlot(ctx, 1, image(MaxSlotImage+1), "x", "", nil); err == nil {
		t.Error("oversized image written")
	}
	if err := s.WriteSlot(ctx, 1, nil, "x", "", nil); err == nil {
		t.Error("empty image written")
	}
	// A radio whose firmware has no slots.
	ns, _ := basicSession(t, func(r *FakeRadio) { r.SetSlots(false) })
	if _, err := ns.SlotInfo(ctx, 1); !errors.Is(err, ErrNoSlots) {
		t.Errorf("no slots: %v", err)
	}
}

// The radio refuses a write from another session (its timestamp).
func TestWriteSlotAuth(t *testing.T) {
	s, radio := basicSession(t, nil)
	s.ts++ // as if another program handshook since
	err := s.WriteSlot(context.Background(), 1, image(10), "x", "", nil)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("refused")) || radio.SlotWrites() != 0 {
		t.Fatalf("err = %v, writes %d", err, radio.SlotWrites())
	}
}

func TestSlotMeta(t *testing.T) {
	for _, c := range []struct{ file, name, version string }{
		{"f4hwn.aprs.v6.1.bin", "Aprs", "v6.1"},
		{"F4HWN.k5v3.fieldops.v7.2.0.bin", "FieldOps", "v7.2.0"},
		{"f4hwn.fm-voice.bin", "Fm Voice", ""},
		{"custom.bin", "", ""},
	} {
		n, v := SlotMeta(c.file, nil)
		if n != c.name || v != c.version {
			t.Errorf("%s: %q %q", c.file, n, v)
		}
	}
	if _, v := SlotMeta("custom.bin", []byte("\x00\x01F4HWN v4.3.2\x00")); v != "v4.3.2" {
		t.Errorf("embedded version %q", v)
	}
	if got := SlotName("Fréquence très longue nom"); got != "Frequence tres " {
		t.Errorf("SlotName = %q", got)
	}
}
