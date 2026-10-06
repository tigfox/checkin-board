package linkcheck

import (
	"strings"
	"testing"
	"time"
)

func dur(d time.Duration) *time.Duration { return &d }

func TestVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    Measure
		want string
	}{
		{"clean", Measure{N: 5, Uplink: 5, RoundTrip: 5, Reply: true, MedianRTT: dur(4 * time.Second)}, Pass},
		{"one lost each way", Measure{N: 5, Uplink: 4, RoundTrip: 4, Reply: true, MedianRTT: dur(6 * time.Second)}, Pass},
		{"slow", Measure{N: 5, Uplink: 5, RoundTrip: 5, Reply: true, MedianRTT: dur(21 * time.Second)}, Marginal},
		{"no reply", Measure{N: 5, Uplink: 5, RoundTrip: 5, MedianRTT: dur(4 * time.Second)}, Marginal},
		{"half", Measure{N: 4, Uplink: 2, RoundTrip: 2, Reply: true, MedianRTT: dur(4 * time.Second)}, Marginal},
		{"uplink only", Measure{N: 5, Uplink: 5, RoundTrip: 1, Reply: true, MedianRTT: dur(4 * time.Second)}, Fail},
		{"dead", Measure{N: 5}, Fail},
		{"60% each way", Measure{N: 5, Uplink: 3, RoundTrip: 2, Reply: true, MedianRTT: dur(5 * time.Second)}, Marginal},
		{"downlink poor", Measure{N: 5, Uplink: 4, RoundTrip: 1, Reply: true, MedianRTT: dur(5 * time.Second)}, Fail},
		{"acks imply heard", Measure{N: 5, Uplink: 0, RoundTrip: 5, Reply: false, MedianRTT: dur(3 * time.Second)}, Marginal},
	} {
		if got := Verdict(tc.m); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestAdvice(t *testing.T) {
	lvl := func(n int) *int { return &n }
	if a := Advice(Marginal, lvl(-3), nil); !strings.Contains(a, "too hot") || !strings.Contains(a, "antenna") {
		t.Errorf("advice = %q", a)
	}
	if a := Advice(Pass, nil, lvl(-45)); !strings.Contains(a, "very low") {
		t.Errorf("advice = %q", a)
	}
	if a := Advice(Pass, lvl(-20), lvl(-22)); a != "" {
		t.Errorf("advice for a good link = %q", a)
	}
}

func TestResponderVerdict(t *testing.T) {
	if v := ResponderVerdict(5, 5, true); v != Pass {
		t.Errorf("full + acked = %s", v)
	}
	if v := ResponderVerdict(5, 5, false); v != Marginal {
		t.Errorf("full, reply not acked = %s", v)
	}
	if v := ResponderVerdict(5, 1, true); v != Fail {
		t.Errorf("1/5 = %s", v)
	}
}
