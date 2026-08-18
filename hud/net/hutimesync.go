package net

import (
	"encoding/binary"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The dash asks the phone for wall-clock time with PXC command 0x10600. The
// generic "acknowledge any even command with cmd+1" path already answers it,
// but with an empty body, which tells the dash nothing. CFDL26-class firmware
// expects the timestamp back; OpenCfMoto sends this exact layout.
const (
	huTimeSyncAckSize   = 45
	huTimeSyncEchoBytes = 16
	huTimeSyncStampLen  = 29
	// "2006-01-02 15:04:05" is 19 bytes; ".mmm" plus six trailing zeroes makes
	// exactly huTimeSyncStampLen, so the stamp never has to be truncated.
	huTimeSyncLayout = "2006-01-02 15:04:05"
	// "2026-08-10" is already 10; anything shorter cannot be a date at all, and
	// treating it as one risks handing the dash back a near-empty field.
	huTimeSyncMinStampChars = 10
)

// huTimeSyncMode names which of the two answers a reply carried, for the log.
const (
	huTimeSyncModeEcho  = "echo"
	huTimeSyncModePhone = "phone"
)

// Finds a year in whatever the dash sent. Deliberately loose: it is NOT a
// validator, only a way to recognise an epoch stamp. It is unanchored at the
// end, accepts either a space or a T between date and time, and says nothing
// about fractional digits - see huTimeSyncShouldEcho for why any stricter
// reading of this field is actively harmful.
var huTimeSyncYearPattern = regexp.MustCompile(
	`(\d{4})-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}`,
)

// huTimeSyncStamp reads the dash's own timestamp out of a request, trimming the
// NUL and space padding firmware pads the field with. Returns "" when the
// request is too short to carry one.
func huTimeSyncStamp(request []byte) string {
	if len(request) <= huTimeSyncEchoBytes {
		return ""
	}
	end := huTimeSyncEchoBytes + huTimeSyncStampLen
	if end > len(request) {
		end = len(request)
	}
	return strings.TrimRight(string(request[huTimeSyncEchoBytes:end]), "\x00 ")
}

// huTimeSyncShouldEcho decides whether to hand the dash its own timestamp back
// untouched instead of replacing it with the phone's.
//
// The rule is deliberately permissive: echo unless the dash sent nothing at all
// or sent epoch. It does NOT require the stamp to parse.
//
// That last point is the whole lesson of this function. An earlier version
// echoed only stamps matching an exact 29-character layout - anchored at both
// ends, a space separator, precisely nine fractional digits - and rewrote
// everything else with phone time. Firmware that formats the field even
// slightly differently (a T separator, a different fraction width, NUL padding)
// therefore had a perfectly good clock overwritten, and the reference
// implementation measured exactly that as clusters landing HOURS out on Voge,
// QJ, X-Cape and Griffin dashes. A stamp we cannot parse is far more likely to
// be a format we have not seen than a clock that needs our help.
func huTimeSyncShouldEcho(request []byte) bool {
	if len(request) < huTimeSyncEchoBytes {
		return false
	}
	stamp := huTimeSyncStamp(request)
	// Printable text of a plausible length, and nothing more. This is NOT a
	// format check - it does not care how the date is punctuated - it only
	// separates "the dash wrote something" from "the field is empty, padding or
	// binary". Without it a short request whose stamp region is a couple of raw
	// bytes would be echoed back as an all-but-empty body, which is the exact
	// shape that put Morini and Voge clusters at 1970 in the first place.
	if len(stamp) < huTimeSyncMinStampChars {
		return false
	}
	for _, c := range []byte(stamp) {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	if strings.TrimSpace(stamp) == "" {
		return false
	}
	if match := huTimeSyncYearPattern.FindStringSubmatch(stamp); match != nil {
		if year, err := strconv.Atoi(match[1]); err == nil && year >= 1969 && year <= 1971 {
			return false
		}
	}
	return true
}

// huTimeSyncAck builds the 45-byte body for a 0x10601 reply, and names which
// answer it gave.
//
// The request is echoed back as far as it goes - the dash correlates on the
// first 16 bytes, and on the echo path its own timestamp bytes are handed back
// exactly as they arrived rather than reformatted. When the request is shorter
// than the header (no dash has been seen doing this, but the frame is
// attacker-adjacent input off a socket, so it cannot be assumed) the header is
// replaced with the same little-endian filler OpenCfMoto uses.
//
// Three behaviours have now been observed in the field, in this order:
//
//   - An EMPTY body left Morini X-Cape and Voge DS800 clusters at epoch/1970.
//     That is why this function exists at all.
//   - Always overwriting with phone time fixed those and broke others.
//   - Overwriting only stamps that failed a strict format check still broke
//     them, by hours, because the check rejected valid stamps it had not been
//     taught. See huTimeSyncShouldEcho.
//
// So: hand back what the dash sent unless it sent nothing or epoch. Never
// empty, in any branch.
func huTimeSyncAck(request []byte, now time.Time) ([]byte, string) {
	out := make([]byte, huTimeSyncAckSize)
	if len(request) >= huTimeSyncEchoBytes {
		copied := len(request)
		if copied > huTimeSyncAckSize {
			copied = huTimeSyncAckSize
		}
		copy(out, request[:copied])
	} else {
		binary.LittleEndian.PutUint32(out[0:4], uint32(0xfffffffe))
		binary.LittleEndian.PutUint32(out[4:8], 0)
		binary.LittleEndian.PutUint32(out[8:12], 1)
		binary.LittleEndian.PutUint32(out[12:16], 0)
	}

	if huTimeSyncShouldEcho(request) {
		return out, huTimeSyncModeEcho
	}

	stamp := fmt.Sprintf("%s.%03d000000", now.Format(huTimeSyncLayout), now.Nanosecond()/int(time.Millisecond))
	copy(out[huTimeSyncEchoBytes:], []byte(stamp))
	return out, huTimeSyncModePhone
}
