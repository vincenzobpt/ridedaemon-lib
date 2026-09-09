package net

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// romeInAugust is UTC+2, so it catches a currentTime that forgot the offset as
// well as one that applied it twice.
func romeInAugust(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skipf("zone database unavailable: %v", err)
	}
	return loc
}

func decodeQueryTime(t *testing.T, body []byte) map[string]any {
	t.Helper()
	if len(body) == 0 {
		t.Fatal("body is empty, which is exactly the reply this replaces")
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	return got
}

func TestQueryTimeAckCarriesEpochOffsetAndZone(t *testing.T) {
	now := time.Date(2026, 8, 2, 14, 46, 32, 163*int(time.Millisecond), romeInAugust(t))

	got := decodeQueryTime(t, queryTimeAck(now, "Europe/Rome", nil))

	if want := float64(now.UnixMilli()); got["time"] != want {
		t.Errorf("time = %v, want %v", got["time"], want)
	}
	// Two hours east in August: the shifted value must lead plain epoch by
	// exactly the offset, never trail it and never match it.
	if want := float64(now.UnixMilli() + 2*60*60*1000); got["currentTime"] != want {
		t.Errorf("currentTime = %v, want %v", got["currentTime"], want)
	}
	if got["currentTimeZone"] != "Europe/Rome" {
		t.Errorf("currentTimeZone = %v, want Europe/Rome", got["currentTimeZone"])
	}
}

func TestQueryTimeAckOmitsDateTimeUnlessTheDashAsked(t *testing.T) {
	now := time.Date(2026, 8, 2, 14, 46, 32, 163*int(time.Millisecond), time.UTC)

	for _, tc := range []struct {
		name string
		conf *HUDConfig
	}{
		{"no handshake yet", nil},
		{"dash did not ask", &HUDConfig{SupportSyncCorrectTime: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeQueryTime(t, queryTimeAck(now, "UTC", tc.conf))
			if _, present := got["dateTime"]; present {
				t.Errorf("dateTime must be absent, got %v", got["dateTime"])
			}
		})
	}
}

func TestQueryTimeAckFormatsDateTimeWhenTheDashAsked(t *testing.T) {
	now := time.Date(2026, 8, 2, 14, 46, 32, 163*int(time.Millisecond), time.UTC)

	for _, tc := range []struct {
		name    string
		channel string
		want    string
	}{
		// Day-first with dots, and milliseconds behind a colon rather than the
		// decimal point a Go layout would produce on its own.
		{"default layout", "37504", "02.08.2026 14:46:32:163"},
		{"colon-date model 21312", "21312", "02:08:2026 14:46:32:163"},
		{"colon-date model 21313", "21313", "02:08:2026 14:46:32:163"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := &HUDConfig{SupportSyncCorrectTime: true, Channel: tc.channel}

			got := decodeQueryTime(t, queryTimeAck(now, "UTC", conf))

			if got["dateTime"] != tc.want {
				t.Errorf("dateTime = %v, want %v", got["dateTime"], tc.want)
			}
		})
	}
}

func TestQueryTimeZoneIDFallsBackToAnOffsetNotTheWordLocal(t *testing.T) {
	// time.Local is what a device gives us, and its name is not a zone id.
	now := time.Date(2026, 8, 2, 14, 46, 32, 0, time.Local)

	got := queryTimeZoneID(now, "")

	if got == "Local" || got == "" {
		t.Fatalf("zone id = %q, which no dash can parse", got)
	}
	if name := now.Location().String(); name != "Local" && got != name {
		t.Errorf("zone id = %q, want the location name %q", got, name)
	}
	if name := now.Location().String(); name == "Local" && !strings.HasPrefix(got, "GMT") {
		t.Errorf("zone id = %q, want a GMT offset fallback", got)
	}
}

func TestQueryTimeZoneIDPrefersTheHostSuppliedID(t *testing.T) {
	now := time.Date(2026, 8, 2, 14, 46, 32, 0, time.UTC)

	if got := queryTimeZoneID(now, "America/New_York"); got != "America/New_York" {
		t.Errorf("zone id = %q, want the host's own value", got)
	}
}

func TestHuTimeLooksLikeUptimeRejectsASetWallClock(t *testing.T) {
	// Field values from SSDQ01-0120 logs on 2026-09-02/08.
	if !huTimeLooksLikeUptime(1581550) {
		t.Error("Moscow reconnect currentHUTime=1581550 must look like uptime")
	}
	if !huTimeLooksLikeUptime(3346122) {
		t.Error("VOGE-5G-dc41 currentHUTime=3346122 must look like uptime")
	}
	if huTimeLooksLikeUptime(1788466877642) {
		t.Error("set-clock currentHUTime=1788466877642 must not look like uptime")
	}
	if !huTimeLooksLikeUptime(0) {
		t.Error("missing/zero currentHUTime must look like uptime")
	}
}

func TestQueryTimeAckIsAnsweredWithCmdPlusOne(t *testing.T) {
	// The reply command is what the dash correlates on; getting it wrong is
	// silent, so it is asserted rather than assumed.
	if PxcQueryTimeAck != PxcQueryTime+1 {
		t.Errorf("ack = %#x, want %#x", PxcQueryTimeAck, PxcQueryTime+1)
	}
	if PxcQueryTime != 0x10450 {
		t.Errorf("QUERY_TIME = %#x, want 0x10450", PxcQueryTime)
	}
}
