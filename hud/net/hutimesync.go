package net

import (
	"encoding/binary"
	"fmt"
	"regexp"
	"strconv"
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
)

// huTimeSyncMode names which of the two answers a reply carried, for the log.
const (
	huTimeSyncModeEcho  = "echo"
	huTimeSyncModePhone = "phone"
)

// The exact shape the dash sends and expects back: seconds resolution, three
// millisecond digits, then six literal zeroes padding the field to nanosecond
// width. Anchored, so anything shorter, longer or differently punctuated is
// rejected rather than partially matched.
var huTimeSyncStampPattern = regexp.MustCompile(
	`^(\d{4})-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{9}$`,
)

// huTimeSyncSaneStamp reports whether a stamp read out of a request is a real
// wall clock rather than epoch, zeroes or junk.
//
// The year bound is what separates "the dash already knows the time" from "the
// dash is telling us it does not". A cluster that has lost its clock reports
// 1970, or all zeroes, or ASCII that is not a timestamp at all; one that is
// running reports the current year. 2020-2099 brackets the service life of
// every dash this protocol appears on without accepting an epoch stamp.
func huTimeSyncSaneStamp(stamp string) bool {
	match := huTimeSyncStampPattern.FindStringSubmatch(stamp)
	if match == nil {
		return false
	}
	year, err := strconv.Atoi(match[1])
	if err != nil {
		return false
	}
	return year >= 2020 && year <= 2099
}

// huTimeSyncAck builds the 45-byte body for a 0x10601 reply, and names which
// answer it gave.
//
// The first 16 bytes are the request's own header echoed straight back - the
// dash correlates on them. When the request is shorter than that (no dash has
// been seen doing this, but the frame is attacker-adjacent input off a socket,
// so it cannot be assumed) the echo is replaced with the same little-endian
// filler OpenCfMoto uses, rather than leaving the bytes zeroed.
//
// The 29-byte stamp that follows is where the care is. Three behaviours have
// now been observed in the field, in this order:
//
//   - An EMPTY body left Morini X-Cape and Voge DS800 clusters at epoch/1970.
//     That is why this function exists at all.
//   - Always overwriting with phone time fixed those, but still left some
//     Zontes and Voge units at 00:00 - the reference implementation shipped
//     that for two releases and then withdrew it.
//   - So: if the dash's own stamp is already a sane wall clock, echo it back
//     untouched. Acknowledging is what the dash needs; correcting a clock that
//     is already right is what breaks it. Phone time is written only when the
//     dash's stamp is missing, epoch, or not a timestamp.
//
// Never empty, in any branch.
func huTimeSyncAck(request []byte, now time.Time) ([]byte, string) {
	out := make([]byte, huTimeSyncAckSize)
	if len(request) >= huTimeSyncEchoBytes {
		copy(out[:huTimeSyncEchoBytes], request[:huTimeSyncEchoBytes])
	} else {
		binary.LittleEndian.PutUint32(out[0:4], uint32(0xfffffffe))
		binary.LittleEndian.PutUint32(out[4:8], 0)
		binary.LittleEndian.PutUint32(out[8:12], 1)
		binary.LittleEndian.PutUint32(out[12:16], 0)
	}

	if len(request) >= huTimeSyncEchoBytes+huTimeSyncStampLen {
		bikeStamp := string(request[huTimeSyncEchoBytes : huTimeSyncEchoBytes+huTimeSyncStampLen])
		if huTimeSyncSaneStamp(bikeStamp) {
			copy(out[huTimeSyncEchoBytes:], bikeStamp)
			return out, huTimeSyncModeEcho
		}
	}

	stamp := fmt.Sprintf("%s.%03d000000", now.Format(huTimeSyncLayout), now.Nanosecond()/int(time.Millisecond))
	copy(out[huTimeSyncEchoBytes:], []byte(stamp))
	return out, huTimeSyncModePhone
}
