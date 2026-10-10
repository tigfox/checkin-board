package graywolf

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// Payloads as graywolf 0.14.14 returned them on a test node (2026-10-09).
const txTimingJSON = `[{"id": 1, "channel": 1, "tx_delay_ms": 300, "tx_tail_ms": 100, "slot_ms": 100, "persist": 63,
  "full_dup": false, "rate_1min": 0, "rate_5min": 0}]`

func TestConfigReadsAndWrites(t *testing.T) {
	f := newFakeGW(t)
	var gotTiming TxTiming
	var gotDigi Digipeater
	var gotPrefs MessagePreferences
	f.mux.HandleFunc("GET /api/tx-timing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(txTimingJSON))
	})
	f.mux.HandleFunc("PUT /api/tx-timing/1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotTiming)
		gotTiming.ID = 1
		writeTestJSON(w, http.StatusOK, gotTiming)
	})
	f.mux.HandleFunc("GET /api/digipeater", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, Digipeater{ID: 1, Enabled: true, DedupeWindowSeconds: 30, MyCall: "KD2DCM-4"})
	})
	f.mux.HandleFunc("PUT /api/digipeater", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotDigi)
		writeTestJSON(w, http.StatusOK, gotDigi)
	})
	f.mux.HandleFunc("PUT /api/messages/preferences", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotPrefs)
		writeTestJSON(w, http.StatusOK, gotPrefs)
	})
	c := f.client(t)
	ctx := context.Background()

	tts, err := c.TxTimings(ctx)
	if err != nil || len(tts) != 1 || tts[0].Channel != 1 || tts[0].TxDelayMS != 300 || tts[0].Persist != 63 {
		t.Fatalf("TxTimings = %+v, %v", tts, err)
	}
	tt := tts[0]
	tt.TxDelayMS = 400
	if _, err := c.SetTxTiming(ctx, tt); err != nil || gotTiming.TxDelayMS != 400 || gotTiming.Persist != 63 || gotTiming.Channel != 1 {
		t.Fatalf("SetTxTiming sent %+v, %v (every field kept)", gotTiming, err)
	}

	d, err := c.Digipeater(ctx)
	if err != nil || !d.Enabled || d.MyCall != "KD2DCM-4" {
		t.Fatalf("Digipeater = %+v, %v", d, err)
	}
	d.Enabled = false
	if _, err := c.SetDigipeater(ctx, d); err != nil || gotDigi.Enabled || gotDigi.DedupeWindowSeconds != 30 || gotDigi.MyCall != "KD2DCM-4" {
		t.Fatalf("SetDigipeater sent %+v, %v", gotDigi, err)
	}

	p := MessagePreferences{DefaultPath: "WIDE1-1", FallbackPolicy: "is_fallback", RetentionDays: 0, RetryMaxAttempts: 4}
	if _, err := c.SetMessagePreferences(ctx, p); err != nil || gotPrefs != p {
		t.Fatalf("SetMessagePreferences sent %+v, %v", gotPrefs, err)
	}
	if _, err := c.SetTxTiming(ctx, TxTiming{}); err == nil {
		t.Fatal("SetTxTiming without an id: want an error")
	}
}
