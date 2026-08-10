package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/net"
)

func TestStopBeforeStartSignalsDone(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)
	if err := hud.StopStream(context.Background()); err != nil {
		t.Fatalf("StopStream() error = %v", err)
	}
	select {
	case <-hud.Done():
	default:
		t.Fatal("StopStream() did not signal Done before stream start")
	}
}

func TestPxcCoordinatorForwardsConfigAndSignalsHeartbeat(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)
	pxc := &net.PXCControl{Events: make(chan net.PXCResponse, 2)}
	ready := make(chan any, 1)
	hud.startPxcEventFwd(pxc, ready)

	payload := json.RawMessage(`{"HUName":"MTX800"}`)
	pxc.Events <- net.PXCResponse{Command: net.PxcHudConf, Body: payload}
	pxc.Events <- net.PXCResponse{Command: net.PxcHeartbeatOk}

	if event := <-hud.Events; event.Source != EventSourcePXC ||
		event.Cmd != int(net.PxcHudConf) ||
		string(event.Data.(json.RawMessage)) != string(payload) {
		t.Fatalf("unexpected PXC config event: %+v", event)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("PXC heartbeat acknowledgement did not signal readiness")
	}
	if event := <-hud.Events; event.Cmd != int(net.PxcHeartbeatOk) {
		t.Fatalf("heartbeat event = %+v, want heartbeat ACK", event)
	}
	close(pxc.Events)
}

func TestSetHostWhenRunningReturnsErrorWithoutPanicking(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)
	hud.running = true
	if err := hud.SetHost(&EcHost{}); err == nil {
		t.Fatal("SetHost() succeeded while the HUD was running")
	}
}

func TestVideoFramingDecisionIsForwardedAsTransportEvent(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)

	hud.handleServerEvent(HudEvent{
		Source: EventSourceTransport,
		Cmd:    TransportCmdVideoFraming,
		Data:   []byte{0, 1},
	})

	event := <-hud.Events
	if event.Source != EventSourceTransport || event.Cmd != TransportCmdVideoFraming {
		t.Fatalf("transport event was rewrapped: %+v", event)
	}
	payload, ok := event.Data.([]byte)
	if !ok || len(payload) != 2 || payload[0] != 0 || payload[1] != 1 {
		t.Fatalf("transport payload = %+v, want [0 1]", event.Data)
	}
}

// The clock answers (the 45-byte 0x10601 body and the 0x10451 dateTime field)
// have shipped for a while, but a Carbit dash only asks a phone that announced
// it can answer. Asserting on the marshalled JSON rather than the struct field
// catches the capability being dropped from the wire as well as being set false.
func TestPhoneConfigAnnouncesClockSyncSupport(t *testing.T) {
	hud := NewCfmotoHUD(30, nil, 0)
	body, err := json.Marshal(hud.phoneConfig)
	if err != nil {
		t.Fatalf("Marshal(phoneConfig) error = %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("Unmarshal(phoneConfig) error = %v", err)
	}
	value, present := sent["supportSyncCorrectTime"]
	if !present {
		t.Fatal("CLIENT_INFO reply omits supportSyncCorrectTime, so the dash is never told the phone can answer its clock questions")
	}
	if value != true {
		t.Errorf("supportSyncCorrectTime = %v, want true", value)
	}
}
