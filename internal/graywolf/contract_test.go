package graywolf

// Contract tests against a real graywolf. They TRANSMIT on RF, so they
// only run when explicitly enabled:
//
//	GW_CONTRACT=1             enable
//	GW_BASE_URL               graywolf to test (default http://localhost:8080)
//	GW_USER / GW_PASSWORD     graywolf login
//	GW_CONTRACT_PEER          callsign to message; must be a station that
//	                          ACKs (e.g. a second graywolf) for the ACK test
//	GW_CONTRACT_SLOW=1        also run the multi-minute retry/ACK timing tests
//	GW_CONTRACT_ACK_WAIT      how long to wait for an ACK (default 3m)
//
// Every test restores the peer's conversation prefs (graywolf drops the
// stored override when the restored values equal its defaults) and
// deletes the rows it sent. Deletes are graywolf soft deletes: the rows
// leave the Messages UI and the API but remain in graywolf's database.

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

type contractEnv struct {
	c    *Client
	peer string
}

func contractSetup(t *testing.T) contractEnv {
	t.Helper()
	if os.Getenv("GW_CONTRACT") != "1" {
		t.Skip("set GW_CONTRACT=1 to run graywolf contract tests (transmits on RF)")
	}
	peer := os.Getenv("GW_CONTRACT_PEER")
	if peer == "" {
		t.Fatal("GW_CONTRACT_PEER is required")
	}
	base := os.Getenv("GW_BASE_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	c, err := New(Config{BaseURL: base, Username: os.Getenv("GW_USER"), Password: os.Getenv("GW_PASSWORD")})
	if err != nil {
		t.Fatal(err)
	}
	return contractEnv{c: c, peer: peer}
}

func requireSlow(t *testing.T) {
	t.Helper()
	if os.Getenv("GW_CONTRACT_SLOW") != "1" {
		t.Skip("set GW_CONTRACT_SLOW=1 for multi-minute timing tests")
	}
}

// withoutRetries turns off graywolf's retry ladder for the peer and
// restores the original prefs when the test ends.
func (e contractEnv) withoutRetries(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	orig, err := e.c.ConversationPrefs(ctx, ThreadKindDM, e.peer)
	if err != nil {
		t.Fatalf("read conversation prefs: %v", err)
	}
	t.Cleanup(func() {
		if _, err := e.c.SetConversationPrefs(context.Background(), ThreadKindDM, e.peer, orig); err != nil {
			t.Errorf("RESTORE FAILED for %s prefs %+v: %v", e.peer, orig, err)
		}
	})
	got, err := e.c.SetConversationPrefs(ctx, ThreadKindDM, e.peer, ConversationPrefs{SendPath: orig.SendPath, WaitForAck: false})
	if err != nil {
		t.Fatalf("set wait_for_ack=false: %v", err)
	}
	if got.WaitForAck {
		t.Fatal("graywolf echoed wait_for_ack=true after setting false")
	}
}

// send transmits a test DM and schedules its deletion.
func (e contractEnv) send(t *testing.T, label string) Message {
	t.Helper()
	text := fmt.Sprintf("CBTEST %s %s", label, time.Now().UTC().Format("150405"))
	m, err := e.c.SendMessage(context.Background(), SendRequest{To: e.peer, Text: text, ClientID: "cbtest-" + label})
	if err != nil {
		t.Fatalf("send %s: %v", label, err)
	}
	t.Cleanup(func() {
		if err := e.c.DeleteMessage(context.Background(), m.ID); err != nil && !IsNotFound(err) {
			t.Errorf("cleanup delete %d: %v", m.ID, err)
		}
	})
	return m
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, err := cond()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(2 * time.Second)
	}
}

func TestContractVersionAndStation(t *testing.T) {
	e := contractSetup(t)
	ctx := context.Background()
	v, err := e.c.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("graywolf version %s on %s", v.Version, v.Platform)
	st, err := e.c.StationConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Callsign == "" {
		t.Error("station callsign is empty")
	}
	t.Logf("station callsign %s", st.Callsign)
}

// graywolf resends a DM only once it has failed (rejected, or attempts
// made and no retry pending). With wait_for_ack=false a row stays at
// attempts 0, so a resend is always refused with 409: the checkpoint
// engine and the link-check responder rely on that and send a
// retransmit as a new message (spec 3.1; contract run 2026-10-10).
func TestContractResendRefusedWithRetriesOff(t *testing.T) {
	e := contractSetup(t)
	e.withoutRetries(t)
	ctx := context.Background()

	// Resend at once, before the peer's ACK can come back over the air
	// (several seconds), so the refusal is the retries-off one.
	m := e.send(t, "resend")
	_, err := e.c.ResendMessage(ctx, m.ID)
	if !IsConflict(err) {
		t.Fatalf("resend with retries off: err = %v, want 409", err)
	}
	t.Logf("resend refused as expected: %v", err)
	got, err := e.c.GetMessage(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == StatusAcked {
		t.Skipf("peer ACKed before the check; 409 covers acked rows too")
	}
	if got.Attempts != 0 || got.NextRetryAt != nil {
		t.Errorf("row = %s attempts %d next_retry_at %v; want attempts 0 and no retry pending", got.Status, got.Attempts, got.NextRetryAt)
	}
}

func TestContractClientIDRoundTrip(t *testing.T) {
	e := contractSetup(t)
	e.withoutRetries(t)
	m := e.send(t, "clientid")
	got, err := e.c.GetMessage(context.Background(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Discovery, not a hard requirement: the design falls back to
	// matching on exact text when client_id isn't persisted.
	t.Logf("FINDING client_id: send response %q, GET %q (sent %q)", m.ClientID, got.ClientID, "cbtest-clientid")
}

func TestContractCursorAndSingleDelete(t *testing.T) {
	e := contractSetup(t)
	e.withoutRetries(t)
	ctx := context.Background()
	params := ListParams{Folder: FolderSent, Peer: e.peer}

	start, err := e.c.CatchUp(ctx, params, func(MessageChange) error { return nil })
	if err != nil {
		t.Fatalf("initial drain: %v", err)
	}
	var ids []uint64
	for i := range 3 {
		ids = append(ids, e.send(t, fmt.Sprintf("cursor%d", i)).ID)
	}

	seen := map[uint64]bool{}
	if _, err := e.c.CatchUp(ctx, withCursor(params, start), func(ch MessageChange) error {
		seen[ch.ID] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("catch-up from saved cursor skipped row %d", id)
		}
	}

	if err := e.c.DeleteMessage(ctx, ids[0]); err != nil {
		t.Fatalf("delete one row: %v", err)
	}
	remaining := map[uint64]bool{}
	if _, err := e.c.CatchUp(ctx, params, func(ch MessageChange) error {
		if ch.Kind != "deleted" {
			remaining[ch.ID] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if remaining[ids[0]] {
		t.Errorf("deleted row %d still listed", ids[0])
	}
	for _, id := range ids[1:] {
		if !remaining[id] {
			t.Errorf("single delete removed sibling row %d from the thread", id)
		}
	}
}

func TestContractSSEReportsOwnSend(t *testing.T) {
	e := contractSetup(t)
	e.withoutRetries(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ids := make(chan uint64, 64)
	streamErr := make(chan error, 1)
	go func() {
		streamErr <- e.c.StreamEvents(ctx, func(ev Event) error {
			select {
			case ids <- ev.Change.ID:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	time.Sleep(time.Second) // let the stream connect before sending
	m := e.send(t, "sse")

	var got []uint64
	for {
		select {
		case id := <-ids:
			got = append(got, id)
			if id == m.ID {
				return
			}
		case err := <-streamErr:
			t.Fatalf("stream ended before row %d appeared (saw %v): %v", m.ID, got, err)
		case <-ctx.Done():
			t.Fatalf("no SSE event for row %d; saw %v", m.ID, got)
		}
	}
}

func TestContractWaitForAckFalseStopsLadder(t *testing.T) {
	e := contractSetup(t)
	requireSlow(t)
	e.withoutRetries(t)
	ctx := context.Background()

	m := e.send(t, "noladder")
	time.Sleep(75 * time.Second) // graywolf's ladder would have fired twice at 30 s
	got, err := e.c.GetMessage(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after 75s: status=%s attempts=%d next_retry_at=%v", got.Status, got.Attempts, got.NextRetryAt)
	if got.Attempts > 1 {
		t.Errorf("attempts = %d, want 1: graywolf retried despite wait_for_ack=false", got.Attempts)
	}
	if got.NextRetryAt != nil {
		t.Errorf("next_retry_at = %v, want none", got.NextRetryAt)
	}
}

func TestContractAckFlipsStatusWithoutLadder(t *testing.T) {
	e := contractSetup(t)
	requireSlow(t)
	e.withoutRetries(t)
	ctx := context.Background()

	wait := 3 * time.Minute
	if v := os.Getenv("GW_CONTRACT_ACK_WAIT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("GW_CONTRACT_ACK_WAIT: %v", err)
		}
		wait = d
	}
	m := e.send(t, "ack")
	terminal := []string{StatusAcked, StatusRejected, StatusFailed, StatusTimeout}
	var last Message
	waitFor(t, wait, "peer ACK", func() (bool, error) {
		cur, err := e.c.GetMessage(ctx, m.ID)
		last = cur
		return slices.Contains(terminal, cur.Status), err
	})
	if last.Status != StatusAcked {
		t.Fatalf("status = %s (%s), want acked", last.Status, last.FailureReason)
	}
	t.Logf("acked after %d attempt(s) at %v", last.Attempts, last.AckedAt)

	// The store releases an acked batch's row before a gap-request
	// resend, on the assumption that resend keeps the acked status.
	if _, err := e.c.ResendMessage(ctx, m.ID); err != nil && !IsConflict(err) {
		t.Fatalf("resend acked row: %v", err)
	}
	after, err := e.c.GetMessage(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("FINDING resend of an acked row: status %s -> %s (store assumes it stays acked)", last.Status, after.Status)
}
