package firmware

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"checkin-board/internal/uvk5"
)

// The slot job writes firmware files from the folder into the radio's
// firmware slots (1-4, external flash) with the radio in normal mode, and
// reads what the slots hold. The radio's main firmware is never written.

// Job states.
const (
	StateIdle    = "idle"
	StateReading = "reading" // the slots
	StateWriting = "writing" // erasing, writing, verifying
	StateDone    = "done"
	StateFailed  = "failed"
)

// readSlotsTimeout bounds reading the four slot headers.
const readSlotsTimeout = 20 * time.Second

// Status is the slot job and the slots as last read.
type Status struct {
	State      string          `json:"state"`
	Step       string          `json:"step,omitempty"` // erasing, writing, verifying
	File       string          `json:"file,omitempty"`
	Slot       int             `json:"slot,omitempty"`
	Name       string          `json:"name,omitempty"`
	Done       int             `json:"done"`
	Total      int             `json:"total"`
	Error      string          `json:"error,omitempty"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Slots      []uvk5.SlotInfo `json:"slots,omitempty"`
	SlotsAt    *time.Time      `json:"slots_at,omitempty"`
}

// SlotSession is what the job uses of the radio (uvk5.Session).
type SlotSession interface {
	SlotInfo(ctx context.Context, slot int) (uvk5.SlotInfo, error)
	WriteSlot(ctx context.Context, slot int, img []byte, name, version string, progress func(uvk5.SlotProgress)) error
	Close() error
}

// SlotsConfig wires a Slots job.
type SlotsConfig struct {
	Folder *Folder
	// Lend takes the radio from the radio backend until giveBack.
	Lend func(ctx context.Context, reason string) (giveBack func(), err error)
	// Connect opens a session on the radio's port (handshake only).
	Connect func(ctx context.Context) (SlotSession, error)
	Logger  *slog.Logger
}

// Slots runs one slot job at a time.
type Slots struct {
	cfg SlotsConfig
	log *slog.Logger

	mu     sync.Mutex
	st     Status
	cancel context.CancelFunc
}

// NewSlots returns an idle slot job.
func NewSlots(cfg SlotsConfig) *Slots {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Slots{cfg: cfg, log: log, st: Status{State: StateIdle}}
}

// ErrBusy: a slot job is already running.
var ErrBusy = errors.New("firmware: the radio's slots are already being read or written")

// Status returns the job and the slots as last read.
func (j *Slots) Status() Status {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := j.st
	st.Slots = append([]uvk5.SlotInfo(nil), j.st.Slots...)
	return st
}

func (j *Slots) begin(st Status) (context.Context, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.st.State == StateReading || j.st.State == StateWriting {
		return nil, ErrBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	st.StartedAt, st.Slots, st.SlotsAt = &now, j.st.Slots, j.st.SlotsAt
	j.st, j.cancel = st, cancel
	return ctx, nil
}

func (j *Slots) finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now().UTC()
	j.st.FinishedAt, j.st.Step = &now, ""
	switch {
	case errors.Is(err, context.Canceled):
		j.st.State, j.st.Error = StateFailed, "cancelled (the slot is left incomplete; the radio won't start it)"
	case err != nil:
		j.st.State, j.st.Error = StateFailed, err.Error()
	default:
		j.st.State = StateDone
	}
	j.cancel()
	j.log.Info("firmware: slot job finished", "file", j.st.File, "slot", j.st.Slot, "state", j.st.State, "err", j.st.Error)
}

// withRadio lends the radio, opens a session, runs fn, and gives the
// radio back.
func (j *Slots) withRadio(ctx context.Context, reason string, fn func(SlotSession) error) error {
	giveBack, err := j.cfg.Lend(ctx, reason)
	if err != nil {
		return fmt.Errorf("can't take the radio: %w", err)
	}
	defer giveBack()
	sess, err := j.cfg.Connect(ctx)
	if err != nil {
		return fmt.Errorf("can't talk to the radio: %w", err)
	}
	defer sess.Close()
	return fn(sess)
}

// readAll reads slots 1-4 and keeps them in the status.
func (j *Slots) readAll(ctx context.Context, sess SlotSession) error {
	var out []uvk5.SlotInfo
	for slot := uvk5.FirstSlot; slot <= uvk5.LastSlot; slot++ {
		info, err := sess.SlotInfo(ctx, slot)
		if err != nil {
			return err
		}
		out = append(out, info)
	}
	now := time.Now().UTC()
	j.mu.Lock()
	j.st.Slots, j.st.SlotsAt = out, &now
	j.mu.Unlock()
	return nil
}

// ReadSlots reads what slots 1-4 hold (the radio is borrowed for a
// second or two).
func (j *Slots) ReadSlots() error {
	ctx, err := j.begin(Status{State: StateReading})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, readSlotsTimeout)
	defer cancel()
	err = j.withRadio(ctx, "reading firmware slots", func(sess SlotSession) error { return j.readAll(ctx, sess) })
	j.finish(err)
	return err
}

// Write starts writing file into slot under name and version; it runs
// on its own (Status shows progress).
func (j *Slots) Write(file string, slot int, name, version string) error {
	if slot < uvk5.FirstSlot || slot > uvk5.LastSlot {
		return fmt.Errorf("%w: slot %d: only slots %d-%d can be written", ErrInvalid, slot, uvk5.FirstSlot, uvk5.LastSlot)
	}
	name = uvk5.SlotName(name)
	if name == "" {
		return fmt.Errorf("%w: give the slot a name (it shows on the radio)", ErrInvalid)
	}
	img, err := j.cfg.Folder.Read(file)
	if err != nil {
		return err
	}
	ctx, err := j.begin(Status{State: StateWriting, Step: "starting", File: file, Slot: slot, Name: name, Total: len(img)})
	if err != nil {
		return err
	}
	j.log.Info("firmware: writing slot", "file", file, "slot", slot, "name", name, "version", version)
	go func() {
		j.finish(j.withRadio(ctx, "writing firmware slot", func(sess SlotSession) error {
			err := sess.WriteSlot(ctx, slot, img, name, version, func(p uvk5.SlotProgress) {
				j.mu.Lock()
				j.st.Step, j.st.Done, j.st.Total = p.Step, p.Done, p.Total
				j.mu.Unlock()
			})
			if err != nil {
				return err
			}
			return j.readAll(ctx, sess) // show the result
		}))
	}()
	return nil
}

// Cancel stops a write between chunks.
func (j *Slots) Cancel() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.st.State != StateWriting {
		return errors.New("firmware: no slot write to cancel")
	}
	j.cancel()
	return nil
}
