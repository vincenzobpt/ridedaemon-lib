package net

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestMediaCaptureAckUsesRequestedCFDL26Dimensions(t *testing.T) {
	request := make([]byte, 204)
	binary.LittleEndian.PutUint16(request[0:2], 720)
	binary.LittleEndian.PutUint16(request[2:4], 712)
	binary.LittleEndian.PutUint32(request[8:12], 2)
	request[29] = 0

	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(request, false), 2, 720, 704, 0)
}

func TestMediaCaptureAckPreservesLegacyNegotiation(t *testing.T) {
	request := make([]byte, 32)
	binary.LittleEndian.PutUint16(request[0:2], 800)
	binary.LittleEndian.PutUint16(request[2:4], 386)
	binary.LittleEndian.PutUint32(request[8:12], 2)
	request[29] = 1

	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(request, false), 2, 800, 384, 1)
}

func TestMediaCaptureAckFallsBackForMissingPayload(t *testing.T) {
	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(nil, false), 2, 800, 384, 1)
}

// The QJ 5-inch dash's exact request: 800x352, wantEncoder=2 (H264), plain framing. With the
// experiment on, the reply must say JPEG while everything else - the 16-aligned geometry the
// dash asked for, and the framing byte it declared - is echoed untouched, because those two
// are not what the experiment is changing.
func TestMediaCaptureAckOffersJpegWhenTheExperimentIsOn(t *testing.T) {
	request := make([]byte, 204)
	binary.LittleEndian.PutUint16(request[0:2], 800)
	binary.LittleEndian.PutUint16(request[2:4], 352)
	binary.LittleEndian.PutUint32(request[8:12], 2)
	request[29] = 0

	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(request, true), 1, 800, 352, 0)
}

// The experiment must never reach a dashboard that did not ask for it: the same request with
// the flag off has to keep answering H.264, or every streaming dash in the fleet changes wire
// format at once.
func TestMediaCaptureAckKeepsH264WhenTheExperimentIsOff(t *testing.T) {
	request := make([]byte, 204)
	binary.LittleEndian.PutUint16(request[0:2], 800)
	binary.LittleEndian.PutUint16(request[2:4], 352)
	binary.LittleEndian.PutUint32(request[8:12], 2)
	request[29] = 0

	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(request, false), 2, 800, 352, 0)
}

func TestMediaStartPreparesConsumerBeforeAcknowledgement(t *testing.T) {
	control := NewMediaControl(":0")
	prepared := false
	control.OnVideoStart = func() { prepared = true }
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		control.handleEvent(&MediaCtrlResponse{Command: MediaCtrlChk}, server)
		close(done)
	}()

	header := make([]byte, mediaCtrlHeaderSize)
	if _, err := io.ReadFull(client, header); err != nil {
		t.Fatalf("read media start acknowledgement: %v", err)
	}
	if !prepared {
		t.Fatal("consumer was not prepared before media start acknowledgement")
	}
	if command := binary.LittleEndian.Uint16(header[0:2]); command != MediaCtrlRcv {
		t.Fatalf("media start acknowledgement command = %d, want %d", command, MediaCtrlRcv)
	}
	_ = client.Close()
	<-done
}

func TestScreenConfigReturnsConfiguredSupportFunction(t *testing.T) {
	control := NewMediaControl(":0")
	control.SupportFunction = 128
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		control.handleEvent(&MediaCtrlResponse{Command: MediaCtrlScreenConf}, server)
		close(done)
	}()

	header := make([]byte, mediaCtrlHeaderSize)
	if _, err := io.ReadFull(client, header); err != nil {
		t.Fatalf("read screen-config response header: %v", err)
	}
	if command := binary.LittleEndian.Uint16(header[0:2]); command != MediaCtrlViewState {
		t.Fatalf("response command = %d, want %d", command, MediaCtrlViewState)
	}
	payload := make([]byte, binary.LittleEndian.Uint16(header[2:4]))
	if _, err := io.ReadFull(client, payload); err != nil {
		t.Fatalf("read screen-config response payload: %v", err)
	}
	var view View
	if err := json.Unmarshal(payload, &view); err != nil {
		t.Fatalf("decode screen-config response: %v", err)
	}
	if view.SupportFunction != 128 {
		t.Fatalf("supportFunction = %d, want 128", view.SupportFunction)
	}
	<-done
}

func assertMediaCaptureAck(
	t *testing.T,
	payload []byte,
	wantEncoder uint32,
	wantWidth uint16,
	wantHeight uint16,
	wantExtended byte,
) {
	t.Helper()
	if len(payload) != 9 {
		t.Fatalf("payload length = %d, want 9", len(payload))
	}
	if got := binary.LittleEndian.Uint32(payload[0:4]); got != wantEncoder {
		t.Fatalf("encoder = %d, want %d", got, wantEncoder)
	}
	if got := binary.LittleEndian.Uint16(payload[4:6]); got != wantWidth {
		t.Fatalf("width = %d, want %d", got, wantWidth)
	}
	if got := binary.LittleEndian.Uint16(payload[6:8]); got != wantHeight {
		t.Fatalf("height = %d, want %d", got, wantHeight)
	}
	if got := payload[8]; got != wantExtended {
		t.Fatalf("extended protocol = %d, want %d", got, wantExtended)
	}
}

// The rule the PXC server learned the hard way, now on this listener too: the bike opens these
// reverse ports more than once and abandons the one it has nothing to say on, so the first socket
// to die here is routinely not the last one alive.
func TestAnAbandonedMediaControlChannelDoesNotEndASessionTheOtherIsStillServing(t *testing.T) {
	control := NewMediaControl(":0")
	live, dashLive := net.Pipe()
	defer live.Close()
	defer dashLive.Close()
	abandoned, dashAbandoned := net.Pipe()
	defer abandoned.Close()

	serveMediaControlConn(t, control, live)
	serveMediaControlConn(t, control, abandoned)
	waitForMediaControlConnections(t, control, 2)

	dashAbandoned.Close()

	err := readMediaControlError(t, control)
	if err.IsFatal() {
		t.Fatalf("an abandoned media control channel ended a session the other was serving: %v", err)
	}
	if !strings.Contains(err.Error(), "still serving the dash") {
		t.Fatalf("the notice a rider can be shown does not say what happened: %v", err)
	}
	waitForMediaControlConnections(t, control, 1)
}

// The other half of the same decision: a dash that really goes away takes every channel with it,
// and that has to go on reading as the session being over.
func TestTheLastMediaControlChannelToDieEndsTheSession(t *testing.T) {
	control := NewMediaControl(":0")
	first, dashFirst := net.Pipe()
	defer first.Close()
	second, dashSecond := net.Pipe()
	defer second.Close()

	serveMediaControlConn(t, control, first)
	serveMediaControlConn(t, control, second)
	waitForMediaControlConnections(t, control, 2)

	dashFirst.Close()
	dashSecond.Close()

	one := readMediaControlError(t, control)
	two := readMediaControlError(t, control)
	if !one.IsFatal() && !two.IsFatal() {
		t.Fatalf("both media control channels died and neither ended the session: %v / %v", one, two)
	}
}

// Every dash that opens this port once keeps the behaviour it always had.
func TestASoleMediaControlChannelFailingIsStillFatal(t *testing.T) {
	control := NewMediaControl(":0")
	conn, dash := net.Pipe()
	defer conn.Close()

	serveMediaControlConn(t, control, conn)
	waitForMediaControlConnections(t, control, 1)

	dash.Close()

	if err := readMediaControlError(t, control); !err.IsFatal() {
		t.Fatalf("the only media control channel died and the session survived it: %v", err)
	}
}

// Our own teardown closes these sockets, and the read it interrupts must not come back as a fault
// of the dash's. Without the guard every stop produced a fatal "use of closed network connection"
// on :10921 - the first line of a field teardown (2026-08-26), reported ahead of the dash-side
// death that actually caused it, because each callback is relayed on its own goroutine and the two
// race. It cost an afternoon to tell which end had really let go.
func TestOurOwnTeardownIsNotReportedAsAMediaControlFault(t *testing.T) {
	// 127.0.0.1, not ":0": a wildcard listener reports its address as "[::]:port", which is not
	// a destination anything can dial.
	control := NewMediaControl("127.0.0.1:0")
	if err := control.Start(); err != nil {
		t.Fatalf("could not start media control: %v", err)
	}
	conn, err := net.Dial("tcp", control.listener.Addr().String())
	if err != nil {
		t.Fatalf("could not reach media control: %v", err)
	}
	defer conn.Close()
	waitForMediaControlConnections(t, control, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = control.Stop(ctx) }()

	select {
	case failure, open := <-control.Errors:
		if open {
			t.Fatalf("our own teardown was reported as a dash fault: %v", failure)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("media control never finished stopping")
	}
}

func serveMediaControlConn(t *testing.T, control *MediaControl, conn net.Conn) {
	t.Helper()
	control.wg.Add(1)
	go control.handleConn(conn)
}

func waitForMediaControlConnections(t *testing.T, control *MediaControl, want int) {
	t.Helper()
	// nil is never a connection this tracker holds, so Others(nil) is the count.
	deadline := time.Now().Add(time.Second)
	for control.tracker.Others(nil) != want {
		if time.Now().After(deadline) {
			t.Fatalf("media control connections = %d, want %d", control.tracker.Others(nil), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func readMediaControlError(t *testing.T, control *MediaControl) FatalError {
	t.Helper()
	select {
	case err := <-control.Errors:
		fatal, ok := err.(FatalError)
		if !ok {
			t.Fatalf("media control error does not say whether it is fatal: %v", err)
		}
		return fatal
	case <-time.After(time.Second):
		t.Fatal("no media control error arrived")
		return nil
	}
}
