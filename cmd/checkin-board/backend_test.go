package main

import (
	"testing"

	"checkin-board/internal/radiobackend"
)

func TestRadioHold(t *testing.T) {
	st := radiobackend.State{}
	hold := radioHold(func() radiobackend.State { return st })
	if hold() != "radio not connected" {
		t.Fatalf("disconnected: %q", hold())
	}
	st.Connected = true
	if hold() != "" {
		t.Fatalf("connected: %q", hold())
	}
}
