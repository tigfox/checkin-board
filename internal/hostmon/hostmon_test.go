package hostmon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)

// fakeProc is a /proc tree with a CPU line and an optional modem process.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) *fakeProc {
	t.Helper()
	p := &fakeProc{t: t, root: t.TempDir()}
	p.write("sys/kernel/arch", "armv6l\n")
	p.write("1/comm", "systemd\n")
	p.write("1/status", "Name:\tsystemd\nvoluntary_ctxt_switches:\t5\n")
	return p
}

func (p *fakeProc) write(rel, body string) {
	p.t.Helper()
	path := filepath.Join(p.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// cpu sets /proc/stat: busy and idle jiffies (user=busy, idle=idle).
func (p *fakeProc) cpu(busy, idle uint64) {
	p.write("stat", fmt.Sprintf("cpu  %d 0 0 %d 0 0 0 0 0 0\ncpu0 %d 0 0 %d 0 0 0 0 0 0\n", busy, idle, busy, idle))
}

func (p *fakeProc) modem(pid int, waits uint64) {
	p.write(fmt.Sprintf("%d/comm", pid), "graywolf-modem\n")
	p.write(fmt.Sprintf("%d/status", pid), fmt.Sprintf("Name:\tgraywolf-modem\nvoluntary_ctxt_switches:\t%d\nnonvoluntary_ctxt_switches:\t99\n", waits))
}

func TestCPUAverageOverFiveMinutes(t *testing.T) {
	p := newFakeProc(t)
	m := New(p.root)
	if s := m.Snapshot(); s.CPUPercent != nil {
		t.Fatalf("CPU before any samples = %v, want unknown", *s.CPUPercent)
	}
	// 10 s apart: 50% busy for 4 minutes, then 100% busy for 2 minutes.
	var busy, idle uint64
	now := t0
	for i := 0; i <= 36; i++ {
		p.cpu(busy, idle)
		m.Sample(now)
		now = now.Add(10 * time.Second)
		if i < 24 {
			busy, idle = busy+500, idle+500
		} else {
			busy += 1000
		}
	}
	// The last 5 minutes are 3 min at 50% then 2 min at 100%:
	// (18*500 + 12*1000) / (30*1000) = 70%.
	s := m.Snapshot()
	if s.CPUPercent == nil || *s.CPUPercent < 69 || *s.CPUPercent > 71 {
		t.Fatalf("CPU 5-min average = %v, want 70%%", s.CPUPercent)
	}
	if s.Machine != "armv6l" {
		t.Errorf("Machine = %q", s.Machine)
	}
}

func TestModemKeepingUp(t *testing.T) {
	p := newFakeProc(t)
	p.cpu(0, 0)
	m := New(p.root)
	m.Sample(t0)
	if s := m.Snapshot(); s.ModemKeepingUp != nil {
		t.Fatal("keeping-up known with no modem process")
	}
	// Waiting for audio ~47 times a second: keeping up, but only once
	// there's close to a minute of history (no verdict from one sample
	// interval, e.g. during boot).
	waits := uint64(1000)
	for i := 1; i <= 7; i++ {
		p.modem(4127, waits)
		m.Sample(t0.Add(time.Duration(i) * 10 * time.Second))
		if s := m.Snapshot(); i < 6 && s.ModemKeepingUp != nil {
			t.Fatalf("verdict after %d s of history: %+v", (i-1)*10, s)
		}
		waits += 470
	}
	s := m.Snapshot()
	if s.ModemKeepingUp == nil || !*s.ModemKeepingUp || s.ModemWaitsPerSec < 46 || s.ModemWaitsPerSec > 48 {
		t.Fatalf("snapshot = %+v, want keeping up at ~47/s", s)
	}
	// Never waiting for a minute (48 kHz on a Pi Zero): behind.
	waits -= 470
	for i := 8; i <= 14; i++ {
		p.modem(4127, waits)
		m.Sample(t0.Add(time.Duration(i) * 10 * time.Second))
	}
	s = m.Snapshot()
	if s.ModemKeepingUp == nil || *s.ModemKeepingUp {
		t.Fatalf("snapshot = %+v, want falling behind", s)
	}
	// A restarted modem (new pid) starts over rather than comparing counts.
	p.write("4127/comm", "other\n")
	p.modem(5000, 3)
	m.Sample(t0.Add(150 * time.Second))
	if s := m.Snapshot(); s.ModemKeepingUp != nil {
		t.Fatalf("snapshot after modem restart = %+v, want unknown", s)
	}
}

// No CPU verdict until the window is nearly full; a counter that went
// backwards (iowait can) is unknown, not a huge figure.
func TestCPUNeedsHistoryAndSaneCounters(t *testing.T) {
	p := newFakeProc(t)
	m := New(p.root)
	p.cpu(1000, 1000)
	m.Sample(t0)
	p.cpu(1900, 1100)
	m.Sample(t0.Add(time.Minute))
	if s := m.Snapshot(); s.CPUPercent != nil {
		t.Fatalf("CPU from 1 minute of history = %v, want unknown", *s.CPUPercent)
	}
	p.cpu(500, 500)
	m.Sample(t0.Add(5 * time.Minute))
	if s := m.Snapshot(); s.CPUPercent != nil {
		t.Fatalf("CPU with counters going backwards = %v, want unknown", *s.CPUPercent)
	}
}

// Without /proc/sys/kernel/arch (older kernels) the machine comes from
// /proc/cpuinfo, so the Pi Zero checks still apply.
func TestMachineFromCPUInfo(t *testing.T) {
	p := newFakeProc(t)
	if err := os.Remove(filepath.Join(p.root, "sys/kernel/arch")); err != nil {
		t.Fatal(err)
	}
	p.write("cpuinfo", "processor\t: 0\nmodel name\t: ARMv6-compatible processor rev 7 (v6l)\nBogoMIPS\t: 697.95\n")
	m := New(p.root)
	m.Sample(t0)
	if got := m.Snapshot().Machine; got != "armv6l" {
		t.Fatalf("Machine = %q, want armv6l", got)
	}
}

// A zombie graywolf-modem (exited, not reaped) isn't the running modem.
func TestZombieModemSkipped(t *testing.T) {
	p := newFakeProc(t)
	p.cpu(0, 0)
	p.write("100/comm", "graywolf-modem\n")
	p.write("100/status", "Name:\tgraywolf-modem\nState:\tZ (zombie)\nvoluntary_ctxt_switches:\t5\n")
	p.modem(200, 1000)
	m := New(p.root)
	m.Sample(t0)
	if m.modemPID != 200 {
		t.Fatalf("modem pid = %d, want the live one (200)", m.modemPID)
	}
}

func TestMissingProcIsUnknownNotError(t *testing.T) {
	m := New(t.TempDir())
	m.Sample(t0)
	m.Sample(t0.Add(10 * time.Second))
	s := m.Snapshot()
	if s.CPUPercent != nil || s.ModemKeepingUp != nil || s.Machine != "" {
		t.Fatalf("snapshot from an empty /proc = %+v, want all unknown", s)
	}
}
