package net

import (
	"bytes"
	"encoding/binary"
	"strings"
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

	got, _ := huTimeSyncAck(request, now)

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

	got, _ := huTimeSyncAck([]byte{0x01, 0x02}, now)

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

// A dash whose clock is already right must get its own stamp back untouched.
// Overwriting it anyway is what left Zontes and Voge clusters at 00:00 for two
// releases of the reference implementation.
func TestHuTimeSyncEchoesASaneBikeStamp(t *testing.T) {
	bikeStamp := "2026-08-10 14:35:12.482000000"
	request := make([]byte, huTimeSyncEchoBytes+huTimeSyncStampLen)
	copy(request[huTimeSyncEchoBytes:], bikeStamp)

	// A phone clock deliberately hours away from the bike's, so an overwrite is
	// unmistakable in the assertion below.
	phoneNow := time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC)
	got, mode := huTimeSyncAck(request, phoneNow)

	if mode != huTimeSyncModeEcho {
		t.Errorf("mode = %q, want %q", mode, huTimeSyncModeEcho)
	}
	if stamp := string(got[huTimeSyncEchoBytes:]); stamp != bikeStamp {
		t.Errorf("stamp = %q, want the bike's own %q - a correct clock must not be corrected", stamp, bikeStamp)
	}
}

func TestHuTimeSyncWritesPhoneTimeOnlyWhenTheDashSentNothingUsable(t *testing.T) {
	phoneNow := time.Date(2026, 8, 10, 14, 35, 12, 482_000_000, time.UTC)
	wantStamp := "2026-08-10 14:35:12.482000000"

	cases := []struct {
		name  string
		stamp string
	}{
		{"epoch", "1970-01-01 00:00:00.000000000"},
		{"epoch with a T separator", "1970-01-01T00:00:00.000000000"},
		{"all zeroes", string(make([]byte, huTimeSyncStampLen))},
		{"binary junk", "\xde\xad\xbe\xef"},
		{"too short to be a date", "26-08-10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := make([]byte, huTimeSyncEchoBytes+huTimeSyncStampLen)
			copy(request[huTimeSyncEchoBytes:], tc.stamp)

			got, mode := huTimeSyncAck(request, phoneNow)
			if mode != huTimeSyncModePhone {
				t.Errorf("mode = %q, want %q", mode, huTimeSyncModePhone)
			}
			if stamp := string(got[huTimeSyncEchoBytes:]); stamp != wantStamp {
				t.Errorf("stamp = %q, want the phone's %q", stamp, wantStamp)
			}
		})
	}
}

// The regression that made this rule permissive: a stamp the phone cannot parse
// is far more likely to be a layout nobody taught it than a clock needing help.
// Rewriting these was measured as clusters landing hours out on four brands.
func TestHuTimeSyncEchoesStampsItCannotParse(t *testing.T) {
	phoneNow := time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC)

	cases := []struct {
		name  string
		stamp string
	}{
		{"T separator", "2026-08-10T14:35:12.482000000"},
		{"fewer fractional digits", "2026-08-10 14:35:12.482"},
		{"no fractional part at all", "2026-08-10 14:35:12"},
		{"slashes instead of dashes", "2026/08/10 14:35:12.482000000"},
		{"a year we would once have called insane", "2019-12-31 23:59:59.000000000"},
		{"trailing NUL padding", "2026-08-10 14:35:12"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := make([]byte, huTimeSyncEchoBytes+huTimeSyncStampLen)
			copy(request[huTimeSyncEchoBytes:], tc.stamp)

			got, mode := huTimeSyncAck(request, phoneNow)
			if mode != huTimeSyncModeEcho {
				t.Fatalf("mode = %q, want %q - this stamp must be handed back untouched", mode, huTimeSyncModeEcho)
			}
			if stamp := strings.TrimRight(string(got[huTimeSyncEchoBytes:]), "\x00"); stamp != tc.stamp {
				t.Errorf("stamp = %q, want the dash's own %q", stamp, tc.stamp)
			}
		})
	}
}
func TestHuTimeSyncAnswersAShortRequestWithPhoneTime(t *testing.T) {
	phoneNow := time.Date(2026, 8, 10, 14, 35, 12, 482_000_000, time.UTC)
	got, mode := huTimeSyncAck(make([]byte, huTimeSyncEchoBytes), phoneNow)

	if mode != huTimeSyncModePhone {
		t.Errorf("mode = %q, want %q", mode, huTimeSyncModePhone)
	}
	if len(got) != huTimeSyncAckSize {
		t.Fatalf("body length = %d, want %d", len(got), huTimeSyncAckSize)
	}
	if stamp := string(got[huTimeSyncEchoBytes:]); stamp != "2026-08-10 14:35:12.482000000" {
		t.Errorf("stamp = %q, want the phone's clock", stamp)
	}
}
