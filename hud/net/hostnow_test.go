package net

import (
	"strings"
	"testing"
	"time"
)

// The bug these cover, from a rider's Voge log on MOTO-HUB 1.1.45 (2026-08-06):
// the QUERY_TIME ack went out at 20:06:56 local carrying
// "05.08.2026 18:06:56:285" and a currentTime equal to plain epoch. Both fields
// were built from time.Now(), whose location on Android is UTC, so the dash was
// told the time two hours wrong while being told the zone was Europe/Rome. The
// existing querytime tests could not see it: they hand in a now that is already
// in the right zone, which is precisely what the device never produced.

func configuredControl(id string, offsetSeconds int) *PXCControl {
	control := &PXCControl{}
	control.SetTimeZoneID(id)
	control.SetTimeZoneOffsetSeconds(offsetSeconds)
	return control
}

func TestHostNowPresentsTheConfiguredZoneWithoutMovingTheInstant(t *testing.T) {
	control := configuredControl("Europe/Rome", 2*60*60)

	before := time.Now().UnixMilli()
	got := control.hostNow()
	after := time.Now().UnixMilli()

	name, offset := got.Zone()
	if name != "Europe/Rome" || offset != 2*60*60 {
		t.Errorf("zone = %q %ds, want \"Europe/Rome\" 7200s", name, offset)
	}
	// Presentation only: the same moment in time, read in another zone.
	if got.UnixMilli() < before || got.UnixMilli() > after {
		t.Errorf("hostNow moved the instant: %d not within [%d,%d]", got.UnixMilli(), before, after)
	}
	if diff := got.Hour() - time.Now().UTC().Hour(); diff != 2 && diff != -22 {
		t.Errorf("wall clock is %dh from UTC, want 2h", diff)
	}
}

func TestHostNowKeepsGoLocalWhenNoOffsetWasConfigured(t *testing.T) {
	control := &PXCControl{}
	control.SetTimeZoneID("Europe/Rome")

	// An id with no offset must not be guessed at: that is the pre-fix shape and
	// silently inventing an offset for it would be worse than leaving it alone.
	if name, offset := control.hostNow().Zone(); name != time.Now().Location().String() && offset != 0 {
		wantName, wantOffset := time.Now().Zone()
		if name != wantName || offset != wantOffset {
			t.Errorf("zone = %q %ds, want local %q %ds", name, offset, wantName, wantOffset)
		}
	}
}

// The end-to-end shape of the field bug: a runtime whose clock reads UTC, a
// dash that asked for dateTime, and a rider two hours east of it.
func TestQueryTimeAckOnUtcRuntimeCarriesTheRidersWallClock(t *testing.T) {
	control := configuredControl("Europe/Rome", 2*60*60)
	utcInstant := time.Date(2026, 8, 5, 18, 6, 56, 285*int(time.Millisecond), time.UTC)
	local := utcInstant.In(time.FixedZone(control.timeZoneID, control.timeZoneOffsetSec))

	control.HudConfig = &HUDConfig{SupportSyncCorrectTime: true}
	got := decodeQueryTime(t, queryTimeAck(local, control.timeZoneID, control.HudConfig))

	dateTime, _ := got["dateTime"].(string)
	if !strings.HasPrefix(dateTime, "05.08.2026 20:06:56") {
		t.Errorf("dateTime = %q, want the 20:06:56 the rider's phone showed, not UTC's 18:06:56", dateTime)
	}
	if want := float64(utcInstant.UnixMilli()); got["time"] != want {
		t.Errorf("time = %v, want plain epoch %v", got["time"], want)
	}
	if want := float64(utcInstant.UnixMilli() + 2*60*60*1000); got["currentTime"] != want {
		t.Errorf("currentTime = %v, want it shifted by the offset to %v", got["currentTime"], want)
	}
}

// 0x10600 is the other clock channel and had the identical defect; no Voge dash
// sends it, but CFMOTO-class firmware does, so it gets the same guarantee.
func TestHuTimeSyncAckCarriesTheRidersWallClock(t *testing.T) {
	utcInstant := time.Date(2026, 8, 5, 18, 6, 56, 285*int(time.Millisecond), time.UTC)
	local := utcInstant.In(time.FixedZone("Europe/Rome", 2*60*60))

	ack, _ := huTimeSyncAck(make([]byte, huTimeSyncEchoBytes), local)
	stamp := string(ack[huTimeSyncEchoBytes:])

	if !strings.HasPrefix(stamp, "2026-08-05 20:06:56") {
		t.Errorf("stamp = %q, want local 20:06:56 rather than UTC 18:06:56", stamp)
	}
}

// The wiring itself: both ack builders must go through hostNow, or the whole
// fix is a formatting change nothing calls. A control configured two hours east
// of UTC must produce a body that says so, without any test-supplied instant.
func TestAckBodiesAreBuiltFromHostNow(t *testing.T) {
	control := configuredControl("Europe/Rome", 2*60*60)
	control.HudConfig = &HUDConfig{SupportSyncCorrectTime: true}

	got := decodeQueryTime(t, control.queryTimeAckBody())
	dateTime, _ := got["dateTime"].(string)
	wantDate := time.Now().In(time.FixedZone("Europe/Rome", 2*60*60)).Format("02.01.2006 15:04")
	if !strings.HasPrefix(dateTime, wantDate) {
		t.Errorf("queryTimeAckBody dateTime = %q, want it to start with the host clock %q", dateTime, wantDate)
	}
	if want := float64(2 * 60 * 60 * 1000); got["currentTime"].(float64)-got["time"].(float64) != want {
		t.Errorf("currentTime - time = %v, want the offset %v", got["currentTime"].(float64)-got["time"].(float64), want)
	}

	body, _ := control.huTimeSyncAckBody(make([]byte, huTimeSyncEchoBytes))
	stamp := string(body[huTimeSyncEchoBytes:])
	wantStamp := time.Now().In(time.FixedZone("Europe/Rome", 2*60*60)).Format("2006-01-02 15:04")
	if !strings.HasPrefix(stamp, wantStamp) {
		t.Errorf("huTimeSyncAckBody stamp = %q, want it to start with the host clock %q", stamp, wantStamp)
	}
}
