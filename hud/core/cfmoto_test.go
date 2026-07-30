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
