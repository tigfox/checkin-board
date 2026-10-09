package epd

import "testing"

func TestControllers(t *testing.T) {
	cs := Controllers()
	if len(cs) != 3 || cs[0] != SSD1680Z {
		t.Fatalf("controllers = %v (newest first)", cs)
	}
	for _, c := range cs {
		if !Known(c) {
			t.Errorf("%s not known", c)
		}
	}
	if Known("") || Known("il0373") {
		t.Error("unknown controller accepted")
	}
}
