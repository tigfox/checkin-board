package graywolf

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// Shape from graywolf's published openapi.yaml (webapi.packetDTO).
const packetsJSON = `[
 {"timestamp":"2026-10-05T06:00:01Z","direction":"RX","type":"message","via":"WIDE1",
  "audio_level":{"level_dbfs":-21.4,"mark":40,"space":38},
  "decoded":{"source":"K1CP","dest":"APGW","message":{"addressee":"N0HQ","text":"RC1 P AS5 7 1/5","messageID":"12"}}},
 {"timestamp":"2026-10-05T06:00:02Z","direction":"RX","type":"message",
  "decoded":{"source":"K1CP","message":{"addressee":"N0HQ","isAck":true,"messageID":"3"}}},
 {"timestamp":"2026-10-05T06:00:03Z","direction":"TX","type":"message","decoded":null}
]`

func TestListPacketsEncodesParamsAndDecodes(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/packets", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for k, v := range map[string]string{"type": "message", "direction": "RX", "since": "2026-10-05T06:00:00Z", "limit": "200"} {
			if q.Get(k) != v {
				t.Errorf("%s = %q, want %q", k, q.Get(k), v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(packetsJSON))
	})
	pk, err := f.client(t).ListPackets(context.Background(), PacketQuery{
		Type: "message", Direction: "RX", Since: time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC), Limit: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pk) != 3 {
		t.Fatalf("packets = %d", len(pk))
	}
	p := pk[0]
	if p.Via != "WIDE1" || p.AudioLevel == nil || p.AudioLevel.LevelDBFS != -21.4 || p.Decoded == nil ||
		p.Decoded.Source != "K1CP" || p.Decoded.Message == nil || p.Decoded.Message.Text != "RC1 P AS5 7 1/5" {
		t.Fatalf("packet = %+v", p)
	}
	if pk[1].AudioLevel != nil || !pk[1].Decoded.Message.IsAck {
		t.Fatalf("ack packet = %+v", pk[1])
	}
	if pk[2].Decoded != nil {
		t.Fatal("undecoded packet has Decoded")
	}
}

func TestListPacketsRejectsBadLimit(t *testing.T) {
	f := newFakeGW(t)
	if _, err := f.client(t).ListPackets(context.Background(), PacketQuery{Limit: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
}
