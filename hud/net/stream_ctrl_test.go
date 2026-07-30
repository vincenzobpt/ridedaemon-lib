package net

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"testing"
)

func TestMediaCaptureAckUsesRequestedCFDL26Dimensions(t *testing.T) {
	request := make([]byte, 204)
	binary.LittleEndian.PutUint16(request[0:2], 720)
	binary.LittleEndian.PutUint16(request[2:4], 712)
	binary.LittleEndian.PutUint32(request[8:12], 2)
	request[29] = 0

	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(request), 2, 720, 704, 0)
}

func TestMediaCaptureAckPreservesLegacyNegotiation(t *testing.T) {
	request := make([]byte, 32)
	binary.LittleEndian.PutUint16(request[0:2], 800)
	binary.LittleEndian.PutUint16(request[2:4], 386)
	binary.LittleEndian.PutUint32(request[8:12], 2)
	request[29] = 1

	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(request), 2, 800, 384, 1)
}

func TestMediaCaptureAckFallsBackForMissingPayload(t *testing.T) {
	assertMediaCaptureAck(t, buildMediaCaptureAckPayload(nil), 2, 800, 384, 1)
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
