package net

import (
	"encoding/binary"
	"encoding/json"
	"io"
	stdnet "net"
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
