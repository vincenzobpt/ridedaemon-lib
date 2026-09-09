package stream

import (
	"testing"
	"time"
)

func TestLiveSourceWaitsForIDR(t *testing.T) {
	source := NewLiveStreamSource(30, 3*time.Second, 3)
	pFrame := annexBAU(1)
	idrFrame := annexBAU(5)

	source.PushFrame(pFrame)
	if frame, _ := source.NextFrame(time.Now()); frame != nil {
		t.Fatal("predictive frame was emitted before the first IDR")
	}

	source.PushFrame(idrFrame)
	frame, err := source.NextFrame(time.Now())
	if err != nil {
		t.Fatalf("read IDR: %v", err)
	}
	if !hasAnnexBNALType(frame, 5) {
		t.Fatal("first emitted frame is not an IDR")
	}
}

func TestPrepareForConsumerDropsQueuedFramesAndWaitsForFreshIDR(t *testing.T) {
	source := NewLiveStreamSource(30, 3*time.Second, 3)
	source.PushFrame(annexBAU(5))
	source.PushFrame(annexBAU(1))
	source.PrepareForConsumer()

	if frame, _ := source.NextFrame(time.Now()); frame != nil {
		t.Fatal("stale frame remained queued after consumer reset")
	}
	source.PushFrame(annexBAU(1))
	if frame, _ := source.NextFrame(time.Now()); frame != nil {
		t.Fatal("predictive frame was emitted while waiting for a fresh IDR")
	}
	source.PushFrame(annexBAU(5))
	if frame, _ := source.NextFrame(time.Now()); !hasAnnexBNALType(frame, 5) {
		t.Fatal("fresh IDR was not emitted after consumer reset")
	}
}

// A JPEG carries no NAL of any type, so the IDR gate the constructor arms would drop every
// still ever pushed - in silence, since a dropped frame is not an error anywhere. This is the
// single failure mode that would make the whole experiment report "JPEG does not work" without
// one JPEG having reached the dash.
func TestOpaqueSourceAcceptsPayloadsThatAreNotAccessUnits(t *testing.T) {
	source := NewLiveStreamSource(10, 3*time.Second, 3)
	source.SetOpaquePayloads(true)

	still := jpegStill(0xAA)
	source.PushFrame(still)

	frame, err := source.NextFrame(time.Now())
	if err != nil {
		t.Fatalf("read still: %v", err)
	}
	if len(frame) != len(still) || frame[0] != 0xFF || frame[len(frame)-1] != 0xAA {
		t.Fatalf("still was not forwarded byte-for-byte: %v", frame)
	}
}

// STREAM_START calls PrepareLiveConsumer, which re-arms the IDR wait. On an opaque source that
// would kill the stream at the exact moment the dash starts pulling - the one place where a
// silent drop looks identical to the black screen the experiment is trying to explain.
func TestOpaqueSourceStaysOpaqueAcrossAConsumerReset(t *testing.T) {
	source := NewLiveStreamSource(10, 3*time.Second, 3)
	source.SetOpaquePayloads(true)
	source.PushFrame(jpegStill(0x01))
	source.PrepareForConsumer()

	source.PushFrame(jpegStill(0x02))
	frame, _ := source.NextFrame(time.Now())
	if frame == nil {
		t.Fatal("a still pushed after the consumer reset was dropped")
	}
	if frame[len(frame)-1] != 0x02 {
		t.Fatalf("the stale still survived the reset: %v", frame)
	}
}

// The dash polls at ~30 Hz while the still source produces ten frames a second, so two polls
// in three find the queue empty. Repeating the last still is what keeps the panel fed; the
// H.264 rule that only an IDR may be repeated has no meaning for a whole picture.
func TestOpaqueSourceRepeatsTheLastStillWhenTheQueueIsEmpty(t *testing.T) {
	source := NewLiveStreamSource(10, 3*time.Second, 3)
	source.SetOpaquePayloads(true)
	source.PushFrame(jpegStill(0x07))

	now := time.Now()
	if frame, _ := source.NextFrame(now); frame == nil {
		t.Fatal("the first still was not emitted")
	}
	repeat, _ := source.NextFrame(now)
	if repeat == nil || repeat[len(repeat)-1] != 0x07 {
		t.Fatalf("an empty queue did not repeat the last still: %v", repeat)
	}
}

// The default must not move: an H.264 session that never asked for stills has to keep dropping
// predictive frames until the first IDR, or every dashboard in the fleet changes behaviour.
func TestSourceKeepsTheIDRGateWhenOpaqueIsOff(t *testing.T) {
	source := NewLiveStreamSource(30, 3*time.Second, 3)
	source.SetOpaquePayloads(false)

	source.PushFrame(annexBAU(1))
	if frame, _ := source.NextFrame(time.Now()); frame != nil {
		t.Fatal("predictive frame was emitted before the first IDR")
	}
}

// A JPEG: SOI, a byte of payload, EOI, then a marker byte the assertions can follow.
func jpegStill(marker byte) []byte {
	return []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0xFF, 0xD9, marker}
}

func annexBAU(nalType byte) []byte {
	return []byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 0, 1, nalType, 1, 2, 3}
}
