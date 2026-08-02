package net

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestHuTimeSyncAckEchoesRequestHeaderAndStampsTime(t *testing.T) {
	request := []byte{
		0x00, 0x06, 0x01, 0x00, 0x2d, 0x00, 0x00, 0x00,
		0x2d, 0x06, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xde, 0xad,
	}
	now := time.Date(2026, 8, 2, 14, 5, 9, 123*int(time.Millisecond), time.UTC)

	got := huTimeSyncAck(request, now)

	if len(got) != 45 {
		t.Fatalf("body must be 45 bytes, got %d", len(got))
	}
	if !bytes.Equal(got[:16], request[:16]) {
		t.Errorf("first 16 bytes must echo the request, got % x", got[:16])
	}
	// A zero-length stamp is the failure this guards: an empty body is exactly
	// what the generic ack already sends, so the test has to assert the text.
	const want = "2026-08-02 14:05:09.123000000"
	if string(got[16:]) != want {
		t.Errorf("stamp = %q, want %q", string(got[16:]), want)
	}
	if len(want) != 29 {
		t.Errorf("stamp layout drifted: %d bytes, want 29", len(want))
	}
}

func TestHuTimeSyncAckFillsHeaderWhenRequestIsShort(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 7*int(time.Millisecond), time.UTC)

	got := huTimeSyncAck([]byte{0x01, 0x02}, now)

	if len(got) != 45 {
		t.Fatalf("body must be 45 bytes, got %d", len(got))
	}
	if v := binary.LittleEndian.Uint32(got[0:4]); v != 0xfffffffe {
		t.Errorf("filler word 0 = %#x, want 0xfffffffe", v)
	}
	if v := binary.LittleEndian.Uint32(got[8:12]); v != 1 {
		t.Errorf("filler word 2 = %d, want 1", v)
	}
	if string(got[16:]) != "2026-01-02 03:04:05.007000000" {
		t.Errorf("stamp = %q", string(got[16:]))
	}
}
