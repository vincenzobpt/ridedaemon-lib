package net

import (
	"encoding/binary"
	"encoding/json"
	"io"
	stdnet "net"
	"strings"
	"testing"
	"time"
)

func TestUnknownEvenPXCRequestWithBodyIsAcknowledged(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	responses := handlePXCEventAndReadResponses(t, control, &PXCResponse{
		Command: PxcOtaFtpInfo,
		Body:    json.RawMessage(`{"port":11021}`),
	}, 1)

	if responses[0].Command != PxcOtaFtpInfo+1 {
		t.Fatalf("ACK command = 0x%x, want 0x%x", responses[0].Command, PxcOtaFtpInfo+1)
	}
	if len(responses[0].Body) != 0 {
		t.Fatalf("ACK body length = %d, want 0", len(responses[0].Body))
	}
	assertNoPXCError(t, control)
}

func TestCheckSnSendsAckAndPositiveResult(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	responses := handlePXCEventAndReadResponses(t, control, &PXCResponse{
		Command: PxcClientSet,
		Body:    json.RawMessage(`{"client_set":"easy_conn","sn":"SERIAL-123"}`),
	}, 2)

	if responses[0].Command != PxcCheckSnAck {
		t.Fatalf("CHECK_SN ACK command = 0x%x, want 0x%x", responses[0].Command, PxcCheckSnAck)
	}
	if responses[1].Command != PxcCheckSnResult {
		t.Fatalf("CHECK_SN result command = 0x%x, want 0x%x", responses[1].Command, PxcCheckSnResult)
	}
	var result checkSnResult
	if err := json.Unmarshal(responses[1].Body, &result); err != nil {
		t.Fatalf("decode CHECK_SN result: %v", err)
	}
	if !result.IsOK || result.ID != "SERIAL-123" || result.ClientSet != "easy_conn" {
		t.Fatalf("unexpected CHECK_SN result: %+v", result)
	}
	assertNoPXCError(t, control)
}

func TestUnknownOddPXCResponseIsNotAcknowledged(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	client, server := stdnet.Pipe()
	defer client.Close()
	defer server.Close()
	if err := server.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	control.handleEvent(&PXCResponse{Command: PxcMediaFeatureConf + 1}, client)
	buffer := make([]byte, 1)
	if _, err := server.Read(buffer); err == nil {
		t.Fatal("unknown odd PXC response unexpectedly produced an ACK")
	}
	assertNoPXCError(t, control)
}

func TestHUDConfigAcceptsSimulatorStringFlavor(t *testing.T) {
	var config HUDConfig
	if err := json.Unmarshal([]byte(`{"HUID":"MOTO-HUB-TBOX-SIMULATOR","flavor":"simulator"}`), &config); err != nil {
		t.Fatalf("decode simulator HUD_CONFIG: %v", err)
	}
	if string(config.Flavor) != `"simulator"` {
		t.Fatalf("flavor = %s, want simulator string", config.Flavor)
	}
}

func TestProactiveHeartbeatRunsOnBothSelectedPxcChannels(t *testing.T) {
	tests := []struct {
		name    string
		command uint32
		wantAck uint32
	}{
		{name: "CAR_CTRL", command: PxcHandshake, wantAck: PxcHandshakeOk},
		{name: "CAR_DATA", command: PxcChannelCarData, wantAck: PxcChannelCarData + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			control := NewPXCControl(":0", nil, nil)
			control.SetProactiveHeartbeat(true)
			control.heartbeatInterval = 5 * time.Millisecond
			responses := handlePXCEventAndReadResponses(
				t,
				control,
				&PXCResponse{Command: test.command},
				2,
			)
			if responses[0].Command != test.wantAck {
				t.Fatalf("channel ACK = 0x%x, want 0x%x", responses[0].Command, test.wantAck)
			}
			if responses[1].Command != PxcHeartbeat || len(responses[1].Body) != 0 {
				t.Fatalf("heartbeat = command 0x%x body %d bytes", responses[1].Command, len(responses[1].Body))
			}
			assertNoPXCError(t, control)
		})
	}
}

func TestProactiveHeartbeatIsOptIn(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	control.heartbeatInterval = 5 * time.Millisecond
	client, server := stdnet.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan struct{})
	go func() {
		control.handleEvent(&PXCResponse{Command: PxcHandshake}, client)
		close(done)
	}()
	if response := readPXCResponse(t, server); response.Command != PxcHandshakeOk {
		t.Fatalf("handshake ACK = 0x%x, want 0x%x", response.Command, PxcHandshakeOk)
	}
	<-done
	if err := server.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(server, make([]byte, pxcHeaderSize)); err == nil {
		t.Fatal("generic profile unexpectedly received a proactive heartbeat")
	}
	assertNoPXCError(t, control)
}

func handlePXCEventAndReadResponses(
	t *testing.T,
	control *PXCControl,
	event *PXCResponse,
	count int,
) []PXCResponse {
	t.Helper()
	client, server := stdnet.Pipe()
	defer client.Close()
	defer server.Close()
	if err := server.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		control.handleEvent(event, client)
		close(done)
	}()

	responses := make([]PXCResponse, 0, count)
	for i := 0; i < count; i++ {
		responses = append(responses, readPXCResponse(t, server))
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PXC handler did not complete")
	}
	return responses
}

func readPXCResponse(t *testing.T, reader io.Reader) PXCResponse {
	t.Helper()
	header := make([]byte, pxcHeaderSize)
	if _, err := io.ReadFull(reader, header); err != nil {
		t.Fatalf("read PXC response header: %v", err)
	}
	response := PXCResponse{
		Command: binary.LittleEndian.Uint32(header[0:4]),
		Size:    binary.LittleEndian.Uint32(header[4:8]),
		Magic:   binary.LittleEndian.Uint32(header[8:12]),
		Token:   binary.LittleEndian.Uint32(header[12:16]),
	}
	if response.Size < pxcHeaderSize {
		t.Fatalf("invalid PXC response size %d", response.Size)
	}
	if response.Magic != response.Size^response.Command {
		t.Fatalf("invalid PXC response magic 0x%x", response.Magic)
	}
	response.Body = make([]byte, int(response.Size)-pxcHeaderSize)
	if _, err := io.ReadFull(reader, response.Body); err != nil {
		t.Fatalf("read PXC response body: %v", err)
	}
	return response
}

func assertNoPXCError(t *testing.T, control *PXCControl) {
	t.Helper()
	select {
	case err := <-control.Errors:
		t.Fatalf("unexpected PXC error: %v", err)
	default:
	}
}

// A rider's diagnostics export is built entirely from control.Events - never
// from this package's own log.Printf lines - so an ack that only writes to the
// wire and never reaches this channel is invisible to everyone except someone
// running a live adb session. That gap is what left an earlier Voge log unable
// to confirm whether HU_TIME_SYNC/QUERY_TIME had actually been answered.
func TestHuTimeSyncEmitsTheAckAsAnEventTooNotJustTheRequest(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	request := &PXCResponse{Command: PxcHuTimeSync, Body: make([]byte, pxcHeaderSize)}
	responses := handlePXCEventAndReadResponses(t, control, request, 1)
	if responses[0].Command != PxcHuTimeSyncAck {
		t.Fatalf("wire ACK command = 0x%x, want 0x%x", responses[0].Command, PxcHuTimeSyncAck)
	}

	requestEvent := <-control.Events
	if requestEvent.Command != PxcHuTimeSync {
		t.Fatalf("first event command = 0x%x, want the request 0x%x", requestEvent.Command, PxcHuTimeSync)
	}
	select {
	case ackEvent := <-control.Events:
		if ackEvent.Command != PxcHuTimeSyncAck {
			t.Fatalf("second event command = 0x%x, want the ack 0x%x", ackEvent.Command, PxcHuTimeSyncAck)
		}
		if len(ackEvent.Body) != huTimeSyncAckSize {
			t.Fatalf("emitted ack body length = %d, want %d", len(ackEvent.Body), huTimeSyncAckSize)
		}
	default:
		t.Fatal("the ack itself was never emitted as an event - a rider's log can never confirm it was sent")
	}
	assertNoPXCError(t, control)
}

func TestQueryTimeEmitsTheAckAsAnEventTooNotJustTheRequest(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	control.HudConfig = &HUDConfig{SupportSyncCorrectTime: true, Channel: "37504"}
	request := &PXCResponse{Command: PxcQueryTime}
	responses := handlePXCEventAndReadResponses(t, control, request, 1)
	if responses[0].Command != PxcQueryTimeAck {
		t.Fatalf("wire ACK command = 0x%x, want 0x%x", responses[0].Command, PxcQueryTimeAck)
	}

	requestEvent := <-control.Events
	if requestEvent.Command != PxcQueryTime {
		t.Fatalf("first event command = 0x%x, want the request 0x%x", requestEvent.Command, PxcQueryTime)
	}
	select {
	case ackEvent := <-control.Events:
		if ackEvent.Command != PxcQueryTimeAck {
			t.Fatalf("second event command = 0x%x, want the ack 0x%x", ackEvent.Command, PxcQueryTimeAck)
		}
		// SupportSyncCorrectTime is set above, so dateTime must ride along - an
		// empty or tiny body here would be the same silent regression this test
		// exists to catch, just on the emitted copy instead of the wire copy.
		var decoded map[string]any
		if err := json.Unmarshal(ackEvent.Body, &decoded); err != nil {
			t.Fatalf("emitted ack body is not JSON: %v", err)
		}
		if _, present := decoded["dateTime"]; !present {
			t.Error("emitted ack body is missing dateTime even though SupportSyncCorrectTime was set")
		}
	default:
		t.Fatal("the ack itself was never emitted as an event - a rider's log can never confirm it was sent")
	}
	assertNoPXCError(t, control)
}

// The bike establishes this port twice - CAR_CTRL and CAR_DATA - and then stops
// servicing the channel it has nothing to say on, which the note at the top of
// pxc.go has called normal since the protocol was first written down. That
// socket dies on its own minutes later, with ETIMEDOUT, while the other channel
// is mid-conversation. Two Voge riders lost the TFT roughly every 18 and every
// 5 minutes to that being reported as the end of the world: in the first of
// those logs the surviving channel had answered a heartbeat 546ms before the
// teardown, and a reconnect two seconds later completed the whole handshake
// against the same dash.
func TestAnAbandonedPxcChannelDoesNotEndASessionTheOtherIsStillServing(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	carCtrl, dashCtrl := stdnet.Pipe()
	defer carCtrl.Close()
	defer dashCtrl.Close()
	carData, dashData := stdnet.Pipe()
	defer carData.Close()

	servePXCConn(t, control, carCtrl)
	servePXCConn(t, control, carData)
	waitForPXCConnections(t, control, 2)

	dashData.Close()

	err := readPXCError(t, control)
	if err.IsFatal() {
		t.Fatalf("an abandoned PXC channel ended a session the other was still serving: %v", err)
	}
	if !strings.Contains(err.Error(), "still serving the dash") {
		t.Fatalf("the notice a rider can be shown does not say what happened: %v", err)
	}
	waitForPXCConnections(t, control, 1)
}

// The other half of the same decision: a dash that really goes away takes every
// channel with it at once, and that must still read as the session being over.
// It is why the connection is retired from the tracker before its failure is
// judged - judged first, two dying channels would each see the other still
// listed and call a real death survivable.
func TestTheLastPxcChannelToDieEndsTheSession(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	carCtrl, dashCtrl := stdnet.Pipe()
	defer carCtrl.Close()
	carData, dashData := stdnet.Pipe()
	defer carData.Close()

	servePXCConn(t, control, carCtrl)
	servePXCConn(t, control, carData)
	waitForPXCConnections(t, control, 2)

	dashCtrl.Close()
	dashData.Close()

	first := readPXCError(t, control)
	second := readPXCError(t, control)
	if !first.IsFatal() && !second.IsFatal() {
		t.Fatalf("both PXC channels died and neither ended the session: %v / %v", first, second)
	}
}

// Every dash that opens this port once keeps the behaviour it always had.
func TestASolePxcChannelFailingIsStillFatal(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	car, dash := stdnet.Pipe()
	defer car.Close()

	servePXCConn(t, control, car)
	waitForPXCConnections(t, control, 1)

	dash.Close()

	if err := readPXCError(t, control); !err.IsFatal() {
		t.Fatalf("the only PXC channel died and the session was left running: %v", err)
	}
}

// A failed answer is judged with the connection still listed - unlike a failed
// read, nothing has retired it, because the loop carries on afterwards. So the
// count has to exclude the connection it is asked about, or the last channel
// left would see itself as company and let the session run on with a dash that
// cannot be written to.
func TestASolePxcChannelThatCannotBeAnsweredIsFatal(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	car, dash := stdnet.Pipe()
	defer car.Close()
	dash.Close()

	control.tracker.Add(car)
	defer control.tracker.Remove(car)

	control.handleEvent(&PXCResponse{Command: PxcHeartbeat}, car)

	if err := readPXCError(t, control); !err.IsFatal() {
		t.Fatalf("the only PXC channel could not be answered and the session was left running: %v", err)
	}
}

func servePXCConn(t *testing.T, control *PXCControl, conn stdnet.Conn) {
	t.Helper()
	control.wg.Add(1)
	go control.handleConn(conn)
}

func waitForPXCConnections(t *testing.T, control *PXCControl, want int) {
	t.Helper()
	// nil is never a connection this tracker holds, so Others(nil) is the count.
	deadline := time.Now().Add(time.Second)
	for control.tracker.Others(nil) != want {
		if time.Now().After(deadline) {
			t.Fatalf("PXC connections = %d, want %d", control.tracker.Others(nil), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func readPXCError(t *testing.T, control *PXCControl) FatalError {
	t.Helper()
	select {
	case err := <-control.Errors:
		fatal, ok := err.(FatalError)
		if !ok {
			t.Fatalf("PXC error does not say whether it is fatal: %v", err)
		}
		return fatal
	case <-time.After(time.Second):
		t.Fatal("no PXC error arrived")
		return nil
	}
}
