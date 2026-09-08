package net

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

// Phone-to-car page control.
//
// Everything else this file answers is dash-originated: the dash asks, we reply with
// cmd+1. These three go the other way. They come from the EasyConn SDK bundled in the
// CarbitRide APK (net.easyconn.carman.sdk_communication.P2C), where the phone sends
// them to move the head unit's own UI and the dash acks each with cmd+1.
//
// NO reference implementation sends any of them - not open-cflink, not open-cfmoto,
// not open-cfmoto-zanderp. They are here as an experiment for one dashboard family
// (Carbit flavor 51, channel 37303) that opens the video socket, pulls the whole
// stream at ~30 Hz and paints none of it, which is what a head unit whose UI never
// left its waiting page looks like from this side. See SetPageSwitchProbe.
const (
	// PxcPageStatus is ECP_P2C_PAGE_STATUS: {"page":n,"status":n,"type":n}. The phone
	// declaring which of its pages is now open - the half missing from a session where
	// the dash drains the stream and shows nothing.
	PxcPageStatus uint32 = 0x20400
	// PxcJumpToCarPage is ECP_P2C_JUMP_TO_CAR_PAGE: {"page":n}. An order rather than a
	// statement; firmware that expects to choose its own page may refuse it.
	PxcJumpToCarPage uint32 = 0x20480
	// PxcSwitchToMainPage is ECP_P2C_SWITCH_TO_SYSTEM_MAIN_PAGE, empty body. Sent last
	// and deliberately: it moves the dash AWAY from mirroring, so it is the control in
	// this experiment. If only this one visibly moves the panel, the P2C page plane
	// works and the mirror page id is what is wrong.
	PxcSwitchToMainPage uint32 = 0x20170
)

// Page ids from ECP_C2P_STANDARD_PAGES in the same SDK.
const (
	EcpPageMain           = 7
	EcpPageMirrorFloating = 21
)

// ECP_APP_PAGE_STATUS_OPEN, from ECP_P2C_PAGE_STATUS.
const ecpPageStatusOpen = 1

// pageSwitchProbeSpacing is how long the probe waits between steps. It has to be long
// enough that a rider watching the panel can say WHICH step moved it - the whole point
// of sending three - and short enough to fit inside the seconds a blank-screen session
// usually lasts before the rider gives up.
const pageSwitchProbeSpacing = 3 * time.Second

// pageProbeEvery is the spacing actually used, so a test can collapse it. Mirrors
// heartbeatEvery.
func (s *PXCControl) pageProbeEvery() time.Duration {
	if s.pageProbeSpacing > 0 {
		return s.pageProbeSpacing
	}
	return pageSwitchProbeSpacing
}

// PageSwitchProbeStep identifies one step of the probe in the event stream.
type PageSwitchProbeStep uint8

const (
	// PageProbeStarted is emitted once, before the first command, so a log can tell a
	// probe that ran and achieved nothing from one that never fired.
	PageProbeStarted PageSwitchProbeStep = iota
	PageProbePageStatus
	PageProbeJumpToCarPage
	PageProbeSwitchToMainPage
	// PageProbeNoControlChannel is emitted instead of the sequence when there is no open
	// PXC connection at all to send it on - named or otherwise, see controlConn.
	PageProbeNoControlChannel
)

// SetPageSwitchProbe enables the phone-to-car page sequence above, once per session,
// after the dash says STREAM_START. Opt-in and off by default: it puts three
// unsolicited commands on the wire that no dashboard has ever been observed to
// receive, so only a profile that has asked for it should ever see them.
func (s *PXCControl) SetPageSwitchProbe(enabled bool) {
	s.pageProbe = enabled
}

// controlConn picks the connection to send the probe on: the one the dash named
// CAR_CTRL, and failing that whichever PXC connection spoke most recently.
//
// The fallback is not defensive padding, it is the case that matters. A channel is only
// named when the dash opens it with PxcHandshake or PxcChannelCarData, and the QJ
// dashboard this probe exists for sends NEITHER - report 59A7-4A36-6C03 carries twenty
// PXC frames across two sessions and not one of them is 0x10000 or 0x20000. Its very
// first frame is CLIENT_INFO on an unnamed connection. Requiring the name would have
// meant the probe reporting "no control channel" on the only dashboard it was written
// for, which is how this nearly shipped.
func (s *PXCControl) controlConn() net.Conn {
	var named, latest net.Conn
	var latestAt int64
	s.connections.Range(func(key, value any) bool {
		state, ok := value.(*pxcConnectionState)
		if !ok {
			return true
		}
		conn, ok := key.(net.Conn)
		if !ok {
			return true
		}
		if name, _ := state.channel.Load().(string); name == "CAR_CTRL" {
			named = conn
			return false
		}
		if at := state.lastRxNanos.Load(); at >= latestAt {
			latestAt, latest = at, conn
		}
		return true
	})
	if named != nil {
		return named
	}
	return latest
}

// StartPageSwitchProbe runs the page sequence once, in the background. Calling it
// again on the same session does nothing, so a dash that says STREAM_START twice does
// not get six commands.
func (s *PXCControl) StartPageSwitchProbe() {
	if !s.pageProbe || s.isStopping() {
		return
	}
	s.pageProbeOnce.Do(func() {
		s.wg.Add(1)
		go s.runPageSwitchProbe()
	})
}

func (s *PXCControl) runPageSwitchProbe() {
	defer s.wg.Done()

	conn := s.controlConn()
	if conn == nil {
		logging.Printf("PXC page-switch probe: no open PXC connection to send on")
		s.notePageProbe(PageProbeNoControlChannel, 0, nil)
		return
	}
	s.notePageProbe(PageProbeStarted, 0, nil)

	mirror, err := json.Marshal(map[string]int{
		"page":   EcpPageMirrorFloating,
		"status": ecpPageStatusOpen,
		"type":   0,
	})
	if err != nil {
		s.emitError(&PxcError{PxcWriteErr, fmt.Errorf("encode PAGE_STATUS: %w", err), true})
		return
	}
	jump, err := json.Marshal(map[string]int{"page": EcpPageMirrorFloating})
	if err != nil {
		s.emitError(&PxcError{PxcWriteErr, fmt.Errorf("encode JUMP_TO_CAR_PAGE: %w", err), true})
		return
	}

	steps := []struct {
		step PageSwitchProbeStep
		cmd  uint32
		body []byte
	}{
		{PageProbePageStatus, PxcPageStatus, mirror},
		{PageProbeJumpToCarPage, PxcJumpToCarPage, jump},
		{PageProbeSwitchToMainPage, PxcSwitchToMainPage, nil},
	}

	for i, step := range steps {
		if i > 0 {
			select {
			case <-s.quit:
				return
			case <-time.After(s.pageProbeEvery()):
			}
		}
		if s.isStopping() {
			return
		}
		err := s.writeResponse(&PXCResponse{Command: step.cmd, Body: step.body}, conn, nil)
		logging.Printf(
			"PXC page-switch probe: sent 0x%x bodyBytes=%d err=%v",
			step.cmd, len(step.body), err,
		)
		s.notePageProbe(step.step, step.cmd, err)
		if err != nil {
			// The channel is gone; the remaining steps would only repeat the same error.
			s.emitError(s.connectionError(conn, PxcWriteErr, err))
			return
		}
	}
}

func (s *PXCControl) notePageProbe(step PageSwitchProbeStep, cmd uint32, err error) {
	if s.OnPageSwitchProbe == nil {
		return
	}
	s.OnPageSwitchProbe(step, cmd, err)
}
