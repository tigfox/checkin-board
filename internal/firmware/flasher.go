package firmware

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"checkin-board/internal/uvk5"
)

// Job states.
const (
	StateIdle     = "idle"
	StateWaiting  = "waiting"  // for the radio in bootloader mode
	StateFlashing = "flashing" // pages going in
	StateDone     = "done"
	StateFailed   = "failed"
)

// DefaultWaitBootloader is how long the operator has to put the radio in
// bootloader mode.
const DefaultWaitBootloader = 3 * time.Minute

// Status is the flash job as the Station page shows it.
type Status struct {
	State      string     `json:"state"`
	File       string     `json:"file,omitempty"`
	Page       int        `json:"page"`
	Pages      int        `json:"pages"`
	Bootloader string     `json:"bootloader,omitempty"`
	Port       string     `json:"port,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Port is an open serial port whose DTR can be raised.
type Port interface {
	io.ReadWriteCloser
	SetDTR(on bool) error
}

// FlasherConfig wires a Flasher. Lend, Ports, Open and Check default to
// nothing useful: production passes the radio backend and uvk5's.
type FlasherConfig struct {
	Folder *Folder
	// Lend takes the radio from the backend until giveBack is called.
	Lend func(ctx context.Context, reason string) (giveBack func(), err error)
	// Ports lists ports the radio may appear on.
	Ports func() []string
	// Check refuses a port that mustn't get DTR (an AIOC).
	Check func(path string) error
	// Open opens a port raw.
	Open func(path string) (Port, error)
	// Timing is the flash protocol's; Wait the bootloader wait.
	Timing uvk5.FlashTiming
	Wait   time.Duration
	Logger *slog.Logger
}

// Flasher runs one flash job at a time.
type Flasher struct {
	cfg FlasherConfig
	log *slog.Logger

	mu     sync.Mutex
	st     Status
	cancel context.CancelFunc
}

// NewFlasher returns an idle flasher.
func NewFlasher(cfg FlasherConfig) *Flasher {
	if cfg.Wait <= 0 {
		cfg.Wait = DefaultWaitBootloader
	}
	if cfg.Timing == (uvk5.FlashTiming{}) {
		cfg.Timing = uvk5.DefaultFlashTiming
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Flasher{cfg: cfg, log: log, st: Status{State: StateIdle}}
}

// ErrBusy: a flash is already running.
var ErrBusy = errors.New("firmware: a flash is already in progress")

// Status returns the current (or last) job.
func (f *Flasher) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

// Start begins flashing name. It returns once the radio is lent and the
// job is waiting for the bootloader; the job then runs on its own.
func (f *Flasher) Start(name string) error {
	fw, err := f.cfg.Folder.Read(name)
	if err != nil {
		return err
	}
	f.mu.Lock()
	if f.st.State == StateWaiting || f.st.State == StateFlashing {
		f.mu.Unlock()
		return ErrBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	f.st, f.cancel = Status{State: StateWaiting, File: name, StartedAt: &now}, cancel
	f.mu.Unlock()
	giveBack, err := f.cfg.Lend(ctx, "flashing firmware")
	if err != nil {
		cancel()
		f.finish(fmt.Errorf("can't take the radio: %w", err))
		return err
	}
	f.log.Info("firmware: flash started; waiting for the bootloader", "file", name)
	go func() {
		defer giveBack()
		defer cancel()
		f.finish(f.run(ctx, fw))
	}()
	return nil
}

// Cancel stops a job that is still waiting for the bootloader. A flash
// in progress isn't stopped: a half-written radio is worse.
func (f *Flasher) Cancel() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.State != StateWaiting {
		return errors.New("firmware: nothing to cancel (a flash in progress can't be stopped)")
	}
	f.cancel()
	return nil
}

func (f *Flasher) finish(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	f.st.FinishedAt = &now
	switch {
	case errors.Is(err, context.Canceled):
		f.st.State, f.st.Error = StateFailed, "cancelled"
	case err != nil:
		f.st.State, f.st.Error = StateFailed, err.Error()
	default:
		f.st.State = StateDone
	}
	f.log.Info("firmware: flash finished", "file", f.st.File, "state", f.st.State, "err", f.st.Error)
}

// run waits for a port with a bootloader on it, then flashes.
func (f *Flasher) run(ctx context.Context, fw []byte) error {
	deadline := time.Now().Add(f.cfg.Wait)
	probe := f.cfg.Timing
	probe.Detect = min(probe.Detect, probeWindow) // per port, per round
	for time.Now().Before(deadline) {
		for _, path := range f.cfg.Ports() {
			if err := ctx.Err(); err != nil {
				return err
			}
			bl, err := f.try(ctx, path, fw, probe)
			switch {
			case err == nil:
				f.log.Info("firmware: flashed", "port", path, "bootloader", bl.Version)
				return nil
			case errors.Is(err, errNotBootloader):
				continue
			default:
				return err
			}
		}
		if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("the radio didn't appear in bootloader mode within %s: switch it off, hold PTT and switch it on, then flash again", f.cfg.Wait)
}

var errNotBootloader = errors.New("not a bootloader")

// probeWindow is how long each port gets to show 5 announcements (UV
// Studio waits 5 s; announcements may come up to 1 s apart).
const probeWindow = 6 * time.Second

// try flashes through path if a bootloader answers there.
func (f *Flasher) try(ctx context.Context, path string, fw []byte, probe uvk5.FlashTiming) (uvk5.Bootloader, error) {
	if err := f.cfg.Check(path); err != nil {
		return uvk5.Bootloader{}, errNotBootloader
	}
	port, err := f.cfg.Open(path)
	if err != nil {
		return uvk5.Bootloader{}, errNotBootloader // gone again, or busy
	}
	defer port.Close()
	_ = port.SetDTR(true) // a USB CDC device may send nothing without it
	timing := f.cfg.Timing
	timing.Detect = probe.Detect
	started := false // past the handshake: from here an error is final
	bl, err := uvk5.Flash(ctx, port, fw, timing, func(p uvk5.FlashProgress) {
		f.mu.Lock()
		started = true
		f.st.State, f.st.Page, f.st.Pages, f.st.Port, f.st.Bootloader = StateFlashing, p.Page, p.Pages, path, p.Bootloader
		f.mu.Unlock()
	})
	f.mu.Lock()
	wrote := started
	f.mu.Unlock()
	switch {
	case err == nil || wrote, errors.Is(err, context.Canceled), errors.Is(err, uvk5.ErrBootloaderTooOld):
		return bl, err
	default:
		// Before the handshake anything (no answer, the port vanishing
		// as the radio restarts) only means "not here yet".
		f.log.Debug("firmware: no bootloader on this port yet", "port", path, "err", err)
		return bl, errNotBootloader
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
