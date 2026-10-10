package hostmon

import (
	"context"
	"testing"
	"time"
)

func TestRunSamplesUntilCancelled(t *testing.T) {
	p := newFakeProc(t)
	p.cpu(100, 100)
	m := New(p.root)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx, time.Millisecond, time.Now); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for m.Snapshot().Machine == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't stop after cancel")
	}
	if m.Snapshot().Machine != "armv6l" {
		t.Fatal("Run never sampled")
	}
}
