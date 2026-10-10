// Package hostmon samples the node's own health from /proc: CPU use and
// whether graywolf's radio modem keeps up with its audio (feedback
// 2026-10-09, items 6 and 15). This is the operating system's data, not
// graywolf's, so reading it doesn't touch graywolf's internals.
//
// "Keeping up" is the most reliable audio-health signal available: a
// modem that keeps up waits for audio between chunks (voluntary context
// switches); one that never waits is behind, and graywolf then drops
// received audio silently. On a Pi Zero at 48 kHz it never waited; at
// 24 kHz it waited ~47 times a second (docs/feedback-2026-10-09.md).
package hostmon

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// CPUWindow is the CPU average's window.
	CPUWindow = 5 * time.Minute
	// ModemWindow is how far back "keeping up" looks.
	ModemWindow = time.Minute
	// minWaitsPerSec: a modem waiting for audio less often than this is
	// treated as behind. Keeping up it waits once per audio chunk (about
	// 12 a second with the Pi Zero build's 2048-frame period at 24 kHz,
	// many more with smaller periods).
	minWaitsPerSec = 1.0
	modemComm      = "graywolf-modem"
	// Verdicts need nearly a full window of history, so a node that just
	// booted (busy, modem still starting) doesn't show false warnings.
	minCPUSpan   = CPUWindow * 9 / 10
	minModemSpan = ModemWindow * 5 / 6
)

// Snapshot is the latest view. Nil pointers mean "not known yet".
type Snapshot struct {
	// CPUPercent is total CPU busy % over the last CPUWindow; nil until
	// nearly a full window has been sampled.
	CPUPercent *float64 `json:"cpu_percent,omitempty"`
	// ModemKeepingUp: graywolf's modem waited for audio in the last
	// ModemWindow. Nil when there is no modem process or under a minute
	// of history.
	ModemKeepingUp *bool `json:"modem_keeping_up,omitempty"`
	// ModemWaitsPerSec is the modem's rate of waiting for audio.
	ModemWaitsPerSec float64 `json:"modem_waits_per_sec,omitempty"`
	// Machine is the kernel's machine name, e.g. "armv6l" (Pi Zero W/Pi 1).
	Machine string `json:"machine,omitempty"`
}

// Monitor keeps a short history of samples. Safe for concurrent use.
type Monitor struct {
	proc string

	mu       sync.Mutex
	cpu      []cpuSample
	modemPID int
	modem    []countSample
	machine  string
}

type cpuSample struct {
	at          time.Time
	busy, total uint64
}

type countSample struct {
	at    time.Time
	waits uint64
}

// New returns a monitor reading the proc filesystem at root ("/proc" on
// a node).
func New(root string) *Monitor { return &Monitor{proc: root} }

// SampleEvery is how often Run samples.
const SampleEvery = 10 * time.Second

// Run samples now and then every interval until ctx ends.
func (m *Monitor) Run(ctx context.Context, every time.Duration, now func() time.Time) {
	m.Sample(now())
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Sample(now())
		}
	}
}

// Sample reads /proc once. Missing files leave that reading unknown.
func (m *Monitor) Sample(now time.Time) {
	busy, total, cpuOK := m.readCPU()
	pid, waits, modemOK := m.readModem()
	machine := m.readMachine()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.machine = machine
	if cpuOK {
		m.cpu = append(trimBefore(m.cpu, now.Add(-CPUWindow-time.Minute), func(s cpuSample) time.Time { return s.at }),
			cpuSample{now, busy, total})
	}
	if !modemOK || pid != m.modemPID {
		m.modem, m.modemPID = nil, pid // gone or restarted: start over
	}
	if modemOK {
		m.modem = append(trimBefore(m.modem, now.Add(-ModemWindow-time.Minute), func(s countSample) time.Time { return s.at }),
			countSample{now, waits})
	}
}

// Snapshot returns the current view.
func (m *Monitor) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{Machine: m.machine}
	if n := len(m.cpu); n >= 2 {
		last := m.cpu[n-1]
		first := since(m.cpu, last.at.Add(-CPUWindow), func(c cpuSample) time.Time { return c.at })
		// Counters going backwards (iowait can) make the reading unknown.
		if last.at.Sub(first.at) >= minCPUSpan && last.total > first.total && last.busy >= first.busy {
			pct := float64(last.busy-first.busy) * 100 / float64(last.total-first.total)
			s.CPUPercent = &pct
		}
	}
	if n := len(m.modem); n >= 2 {
		last := m.modem[n-1]
		first := since(m.modem, last.at.Add(-ModemWindow), func(c countSample) time.Time { return c.at })
		if span := last.at.Sub(first.at); span >= minModemSpan && last.waits >= first.waits {
			s.ModemWaitsPerSec = float64(last.waits-first.waits) / span.Seconds()
			ok := s.ModemWaitsPerSec >= minWaitsPerSec
			s.ModemKeepingUp = &ok
		}
	}
	return s
}

// since returns the window's starting sample: the newest at or before
// t, or the oldest when none is that old yet.
func since[T any](xs []T, t time.Time, at func(T) time.Time) T {
	start := xs[0]
	for _, x := range xs {
		if at(x).After(t) {
			break
		}
		start = x
	}
	return start
}

func trimBefore[T any](xs []T, t time.Time, at func(T) time.Time) []T {
	i := 0
	for i < len(xs) && at(xs[i]).Before(t) {
		i++
	}
	return append([]T(nil), xs[i:]...)
}

// readCPU sums /proc/stat's aggregate "cpu" line: busy is everything but
// idle and iowait.
func (m *Monitor) readCPU() (busy, total uint64, ok bool) {
	b, err := os.ReadFile(filepath.Join(m.proc, "stat"))
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := bytes.Cut(b, []byte("\n"))
	f := strings.Fields(string(line))
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, false
	}
	for i, v := range f[1:] {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += n
		if i != 3 && i != 4 { // idle, iowait
			busy += n
		}
	}
	return busy, total, true
}

// readModem finds graywolf's (live) modem process and its voluntary
// context switches (times it waited, e.g. for audio).
//
// Only the main thread's count (/proc/PID/status) is read, on purpose:
// graywolf's modem demodulates on its main thread (92% of a Pi Zero's
// CPU on the node, 2026-10-09), while its ALSA audio thread wakes every
// period even when the main thread is behind. Summing every thread
// (/proc/PID/task/*) would hide exactly the falling-behind this detects.
// Recheck if a graywolf release moves demodulation to a worker thread.
func (m *Monitor) readModem() (pid int, waits uint64, ok bool) {
	entries, err := os.ReadDir(m.proc)
	if err != nil {
		return 0, 0, false
	}
	for _, e := range entries {
		p, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(m.proc, e.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != modemComm {
			continue
		}
		w, live, ok := readStatus(filepath.Join(m.proc, e.Name(), "status"))
		if !live {
			continue // a zombie (exited, not yet reaped) isn't the modem
		}
		return p, w, ok
	}
	return 0, 0, false
}

// readStatus returns a process's voluntary context switches and whether
// it is alive (not a zombie or dead).
func readStatus(path string) (waits uint64, live, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, false
	}
	defer f.Close()
	live = true
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, found := strings.CutPrefix(line, "State:"); found {
			st := strings.TrimSpace(v)
			live = !strings.HasPrefix(st, "Z") && !strings.HasPrefix(st, "X")
		}
		if v, found := strings.CutPrefix(line, "voluntary_ctxt_switches:"); found {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			waits, ok = n, err == nil
		}
	}
	return waits, live, ok
}

// cpuArchRe finds the ARM architecture in /proc/cpuinfo's model name,
// e.g. "ARMv6-compatible processor rev 7 (v6l)".
var cpuArchRe = regexp.MustCompile(`\((v[0-9]+l)\)`)

// readMachine is the kernel's machine name (uname -m): from
// /proc/sys/kernel/arch, or, where a kernel lacks it, /proc/cpuinfo.
func (m *Monitor) readMachine() string {
	if b, err := os.ReadFile(filepath.Join(m.proc, "sys", "kernel", "arch")); err == nil {
		return strings.TrimSpace(string(b))
	}
	b, err := os.ReadFile(filepath.Join(m.proc, "cpuinfo"))
	if err != nil {
		return ""
	}
	if mm := cpuArchRe.FindSubmatch(b); mm != nil {
		return "arm" + string(mm[1])
	}
	return ""
}
