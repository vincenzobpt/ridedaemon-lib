package net

import (
	"encoding/binary"
	"fmt"
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

// huTimeSyncAck builds the 45-byte body for a 0x10601 reply.
//
// The first 16 bytes are the request's own header echoed straight back - the
// dash correlates on them. When the request is shorter than that (no dash has
// been seen doing this, but the frame is attacker-adjacent input off a socket,
// so it cannot be assumed) the echo is replaced with the same little-endian
// filler OpenCfMoto uses, rather than leaving the bytes zeroed.
func huTimeSyncAck(request []byte, now time.Time) []byte {
	out := make([]byte, huTimeSyncAckSize)
	if len(request) >= huTimeSyncEchoBytes {
		copy(out[:huTimeSyncEchoBytes], request[:huTimeSyncEchoBytes])
	} else {
		binary.LittleEndian.PutUint32(out[0:4], uint32(0xfffffffe))
		binary.LittleEndian.PutUint32(out[4:8], 0)
		binary.LittleEndian.PutUint32(out[8:12], 1)
		binary.LittleEndian.PutUint32(out[12:16], 0)
	}
	stamp := fmt.Sprintf("%s.%03d000000", now.Format(huTimeSyncLayout), now.Nanosecond()/int(time.Millisecond))
	copy(out[huTimeSyncEchoBytes:], []byte(stamp))
	return out
}
