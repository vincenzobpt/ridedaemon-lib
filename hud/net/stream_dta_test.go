package net

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
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

// Same rule as the PXC and media control listeners: one socket dying is only the end of the
// session when nothing else is being served here.
func TestAnAbandonedVideoChannelDoesNotEndASessionTheOtherIsStillServing(t *testing.T) {
	stream := NewMediaStream(":0", nil, 0x1000, time.Millisecond)
	live, dashLive := net.Pipe()
	defer live.Close()
	defer dashLive.Close()
	abandoned, dashAbandoned := net.Pipe()
	defer abandoned.Close()

	serveVideoConn(t, stream, live)
	serveVideoConn(t, stream, abandoned)
	waitForVideoConnections(t, stream, 2)

	dashAbandoned.Close()

	err := readVideoError(t, stream)
	if err.IsFatal() {
		t.Fatalf("an abandoned video channel ended a session the other was serving: %v", err)
	}
	if !strings.Contains(err.Error(), "still serving the dash") {
		t.Fatalf("the notice a rider can be shown does not say what happened: %v", err)
	}
}

// A dash that closes the video socket while nothing else is being served has ended the session,
// and the sentence it produces is the one every earlier build produced - failures are grouped
// downstream by that text.
func TestASoleVideoChannelClosingIsStillFatal(t *testing.T) {
	stream := NewMediaStream(":0", nil, 0x1000, time.Millisecond)
	conn, dash := net.Pipe()
	defer conn.Close()

	serveVideoConn(t, stream, conn)
	waitForVideoConnections(t, stream, 1)

	dash.Close()

	err := readVideoError(t, stream)
	if !err.IsFatal() {
		t.Fatalf("the only video channel closed and the session survived it: %v", err)
	}
	if !strings.Contains(err.Error(), "error reading header: EOF") {
		t.Fatalf("the fatal sentence changed, which regroups every rider's history: %v", err)
	}
}

func TestOurOwnTeardownIsNotReportedAsAVideoFault(t *testing.T) {
	// 127.0.0.1, not ":0" - see the media control test's note.
	stream := NewMediaStream("127.0.0.1:0", nil, 0x1000, time.Millisecond)
	if err := stream.Start(); err != nil {
		t.Fatalf("could not start media stream: %v", err)
	}
	conn, err := net.Dial("tcp", stream.listener.Addr().String())
	if err != nil {
		t.Fatalf("could not reach media stream: %v", err)
	}
	defer conn.Close()
	waitForVideoConnections(t, stream, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = stream.Stop(ctx) }()

	select {
	case failure, open := <-stream.Errors:
		if open {
			t.Fatalf("our own teardown was reported as a dash fault: %v", failure)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("media stream never finished stopping")
	}
}

func serveVideoConn(t *testing.T, stream *MediaStream, conn net.Conn) {
	t.Helper()
	stream.wg.Add(1)
	go stream.handleConn(conn)
}

func waitForVideoConnections(t *testing.T, stream *MediaStream, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for stream.tracker.Others(nil) != want {
		if time.Now().After(deadline) {
			t.Fatalf("video connections = %d, want %d", stream.tracker.Others(nil), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func readVideoError(t *testing.T, stream *MediaStream) FatalError {
	t.Helper()
	select {
	case err := <-stream.Errors:
		fatal, ok := err.(FatalError)
		if !ok {
			t.Fatalf("video error does not say whether it is fatal: %v", err)
		}
		return fatal
	case <-time.After(time.Second):
		t.Fatal("no video error arrived")
		return nil
	}
}
