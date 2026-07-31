package net

import (
	"encoding/binary"
	"testing"
)

var testAccessUnit = []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84}

func TestFramedPacketKeepsTheIndexByDefault(t *testing.T) {
	frame, idx := buildFramedPacket(testAccessUnit, 7, true)

	if idx != 7 {
		t.Fatalf("index = %d, want 7", idx)
	}
	if got := binary.LittleEndian.Uint32(frame[0:4]); got != uint32(len(testAccessUnit)+4) {
		t.Fatalf("length = %d, want %d", got, len(testAccessUnit)+4)
	}
	if got := binary.LittleEndian.Uint32(frame[4:8]); got != 7 {
		t.Fatalf("framed index = %d, want 7", got)
	}
	if got := string(frame[8:]); got != string(testAccessUnit) {
		t.Fatalf("access unit = % x, want % x", frame[8:], testAccessUnit)
	}
}

func TestFramedPacketDropsTheIndexWhenPlain(t *testing.T) {
	frame, _ := buildFramedPacket(testAccessUnit, 7, false)

	if got := binary.LittleEndian.Uint32(frame[0:4]); got != uint32(len(testAccessUnit)) {
		t.Fatalf("length = %d, want %d", got, len(testAccessUnit))
	}
	// The access unit must start immediately after the length, so the dash hands a
	// clean Annex-B buffer to its decoder.
	if got := string(frame[4:]); got != string(testAccessUnit) {
		t.Fatalf("access unit = % x, want % x", frame[4:], testAccessUnit)
	}
}

func TestPlainFramingIsOptIn(t *testing.T) {
	s := NewMediaStream(":0", nil, 0, 0)

	// A dash reporting supportExtendProtocol=0 changes nothing until the host says
	// this dashboard is unidentified. This is what keeps every recognised profile on
	// the exact wire format it works with today.
	if s.NegotiatedExtendedProtocol(false) {
		t.Fatal("negotiation reported plain framing without the host allowing it")
	}
	if s.plainFraming.Load() {
		t.Fatal("plain framing engaged without the host allowing it")
	}

	s.SetPlainFramingAllowed(true)
	if !s.NegotiatedExtendedProtocol(false) {
		t.Fatal("negotiation did not report plain framing for a dash reporting extend=0")
	}
	if !s.plainFraming.Load() {
		t.Fatal("plain framing did not engage for a dash reporting extend=0")
	}
}

func TestAllowedPlainFramingStillHonoursAnExtendedDash(t *testing.T) {
	s := NewMediaStream(":0", nil, 0, 0)
	s.SetPlainFramingAllowed(true)

	if s.NegotiatedExtendedProtocol(true) {
		t.Fatal("negotiation reported plain framing for a dash reporting extend=1")
	}
	if s.plainFraming.Load() {
		t.Fatal("dash reporting extend=1 was switched to plain framing")
	}
}
