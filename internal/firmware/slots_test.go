package firmware

import (
	"bytes"
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"checkin-board/internal/uvk5"
)

type slotBench struct {
	radio *uvk5.FakeRadio
	lent  atomic.Int32
	given atomic.Int32
}

func (b *slotBench) config(fo *Folder) SlotsConfig {
	return SlotsConfig{
		Folder: fo,
		Lend: func(context.Context, string) (func(), error) {
			b.lent.Add(1)
			return func() { b.given.Add(1) }, nil
		},
		Connect: func(ctx context.Context) (SlotSession, error) {
			host, dev := net.Pipe()
			go func() { _ = b.radio.Serve(dev) }() // ends when the session closes the port, as a real one
			return uvk5.ConnectBasic(ctx, host)
		},
	}
}

func waitSlots(t *testing.T, j *Slots, want ...string) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := j.Status()
		for _, w := range want {
			if st.State == w {
				return st
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state = %+v, want %v", j.Status(), want)
	return Status{}
}

func TestSlotsReadAndWrite(t *testing.T) {
	fo := NewFolder(filepath.Join(t.TempDir(), "fw"))
	img := bytes.Repeat([]byte{0x5A, 0xA5}, 700)
	if err := fo.Save("f4hwn.aprs.v6.1.bin", img, false); err != nil {
		t.Fatal(err)
	}
	if l, _ := fo.List(); len(l) != 1 || l[0].SlotName != "Aprs" || l[0].SlotVersion != "v6.1" {
		t.Fatalf("list = %+v", l)
	}
	b := &slotBench{radio: uvk5.NewFakeRadio("KD2DCM-4", time.Now)}
	j := NewSlots(b.config(fo))
	if err := j.ReadSlots(); err != nil {
		t.Fatal(err)
	}
	st := j.Status()
	if st.State != StateDone || len(st.Slots) != 4 || st.Slots[0].Slot != 1 || st.Slots[0].State != "empty" || b.given.Load() != 1 {
		t.Fatalf("read = %+v", st)
	}
	for _, bad := range []int{0, 5} {
		if err := j.Write("f4hwn.aprs.v6.1.bin", bad, "Aprs", "v6.1", false); !errors.Is(err, ErrInvalid) {
			t.Fatalf("slot %d: %v", bad, err)
		}
	}
	if err := j.Write("f4hwn.aprs.v6.1.bin", 2, " ", "", false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no name: %v", err)
	}
	if err := j.Write("f4hwn.aprs.v6.1.bin", 2, "Aprs", "v6.1", false); err != nil {
		t.Fatal(err)
	}
	st = waitSlots(t, j, StateDone, StateFailed)
	if st.State != StateDone || st.Done != len(img) || st.Slots[1].State != "valid" || st.Slots[1].Name != "Aprs" {
		t.Fatalf("write = %+v", st)
	}
	if info, stored := b.radio.Slot(2); info.Version != "v6.1" || !bytes.Equal(stored, img) {
		t.Fatalf("radio slot = %+v", info)
	}
	if b.lent.Load() != 2 || b.given.Load() != 2 {
		t.Fatalf("lent %d, given back %d", b.lent.Load(), b.given.Load())
	}
	if err := j.Cancel(); err == nil {
		t.Fatal("cancel with nothing running")
	}
}

// A radio whose firmware has no slots: the job says so and gives the
// radio back.
func TestSlotsNoMultiboot(t *testing.T) {
	fo := NewFolder(filepath.Join(t.TempDir(), "fw"))
	b := &slotBench{radio: uvk5.NewFakeRadio("KD2DCM-4", time.Now)}
	b.radio.SetSlots(false)
	j := NewSlots(b.config(fo))
	if err := j.ReadSlots(); !errors.Is(err, uvk5.ErrNoSlots) {
		t.Fatalf("err = %v", err)
	}
	if st := j.Status(); st.State != StateFailed || b.given.Load() != 1 {
		t.Fatalf("status %+v, given %d", st, b.given.Load())
	}
}

// A slot holding Fusion (the radio's main firmware) isn't overwritten
// unless asked; the check is made on the radio just before erasing.
func TestSlotsProtectFusion(t *testing.T) {
	fo := NewFolder(filepath.Join(t.TempDir(), "fw"))
	img := bytes.Repeat([]byte{1}, 300)
	_ = fo.Save("f4hwn.aprs.v6.2.bin", img, false)
	b := &slotBench{radio: uvk5.NewFakeRadio("KD2DCM-4", time.Now)}
	j := NewSlots(b.config(fo))
	if err := j.Write("f4hwn.aprs.v6.2.bin", 1, "Fusion", "v6.1.0", false); err != nil { // the bench's layout
		t.Fatal(err)
	}
	waitSlots(t, j, StateDone)
	if err := j.Write("f4hwn.aprs.v6.2.bin", 1, "APRS", "v6.2", false); err != nil {
		t.Fatal(err)
	}
	if st := waitSlots(t, j, StateDone, StateFailed); st.State != StateFailed || !strings.Contains(st.Error, "Fusion") {
		t.Fatalf("over Fusion = %+v", st)
	}
	if info, _ := b.radio.Slot(1); info.Name != "Fusion" {
		t.Fatalf("slot 1 = %+v", info)
	}
	if err := j.Write("f4hwn.aprs.v6.2.bin", 1, "APRS", "v6.2", true); err != nil {
		t.Fatal(err)
	}
	if st := waitSlots(t, j, StateDone, StateFailed); st.State != StateDone {
		t.Fatalf("asked to replace = %+v", st)
	}
}

// A cancelled write re-reads the slots: the table shows the slot as it
// now is (empty: the header that commits it comes last).
func TestSlotsCancelRereads(t *testing.T) {
	fo := NewFolder(filepath.Join(t.TempDir(), "fw"))
	_ = fo.Save("f4hwn.aprs.v6.1.bin", bytes.Repeat([]byte{7}, uvk5.MaxSlotImage), false)
	b := &slotBench{radio: uvk5.NewFakeRadio("KD2DCM-4", time.Now)}
	j := NewSlots(b.config(fo))
	if err := j.Write("f4hwn.aprs.v6.1.bin", 2, "APRS", "v1", false); err != nil {
		t.Fatal(err)
	}
	for j.Status().Done == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := j.Cancel(); err != nil {
		t.Fatal(err)
	}
	st := waitSlots(t, j, StateFailed)
	if !strings.Contains(st.Error, "cancelled") || len(st.Slots) != 4 || st.Slots[1].State != "empty" {
		t.Fatalf("after cancel = %+v", st)
	}
}
