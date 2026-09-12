package net

import (
	"encoding/json"
	"testing"
)

// The body has to match kh.b in the CarbitRide APK field for field: a head unit that
// reads a key we spelled differently learns nothing, and this command exists precisely
// to be understood.
func TestAppStatusBodyMatchesTheOfficialShape(t *testing.T) {
	body, err := buildAppStatusBody(PhoneScreen{Width: 1080, Height: 2400, Rotation: 0}, AppStatusMirrorLive)
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, key := range []string{"mode", "displayRotation", "width", "height", "enableAccessibility", "enableAOAHid"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("body is missing %q: %s", key, body)
		}
	}
	if len(got) != 6 {
		t.Fatalf("body has %d keys, want the 6 kh.b writes: %s", len(got), body)
	}
}

// kh.b's constructor puts the long side in "height" when the phone is upright and in
// "width" when it is on its side, which is what rotation 1 and 3 mean.
func TestAppStatusReportsTheScreenTheWayTheRotationDoes(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		rotation              int
		wantWidth, wantHeight float64
	}{
		{"upright", 0, 1080, 2400},
		{"rotated left", 1, 2400, 1080},
		{"upside down", 2, 1080, 2400},
		{"rotated right", 3, 2400, 1080},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := buildAppStatusBody(PhoneScreen{Width: 1080, Height: 2400, Rotation: tc.rotation}, AppStatusMirrorLive)
			if err != nil {
				t.Fatalf("build body: %v", err)
			}
			got := decodeNumbers(t, body)
			if got["width"] != tc.wantWidth || got["height"] != tc.wantHeight {
				t.Fatalf("size = %vx%v, want %vx%v", got["width"], got["height"], tc.wantWidth, tc.wantHeight)
			}
			if got["displayRotation"] != float64(tc.rotation) {
				t.Fatalf("displayRotation = %v, want %d", got["displayRotation"], tc.rotation)
			}
		})
	}
}

// kh.b.a(2) overrides the geometry it was constructed with: mode 2 always describes the
// phone upright. Getting this wrong would tell a dash the phone is landscape at the one
// moment the official app insists it is not.
func TestAppStatusBackgroundModeIsAlwaysUpright(t *testing.T) {
	body, err := buildAppStatusBody(PhoneScreen{Width: 1080, Height: 2400, Rotation: 3}, AppStatusBackground)
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	got := decodeNumbers(t, body)
	if got["displayRotation"] != 0 || got["width"] != 1080 || got["height"] != 2400 {
		t.Fatalf("mode 2 body = %s, want rotation 0 and 1080x2400", body)
	}
}

// Off by default, and silent when off: every dashboard in the fleet that paints a
// picture today does it without ever having seen this command from us.
func TestAppStatusStaysOffUnlessAsked(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	sent := 0
	control.OnAppStatus = func(int, error) { sent++ }
	control.SendAppStatus(AppStatusMirrorLive)
	if sent != 0 {
		t.Fatalf("OnAppStatus fired %d times with the notification off", sent)
	}
}

// A dash that says STREAM_START twice must not be told twice that the mirror went live -
// the same rule the page probe needed, for the same firmware.
func TestAppStatusSendsEachModeOnce(t *testing.T) {
	control := NewPXCControl(":0", nil, nil)
	control.SetAppStatusNotify(true)
	var modes []int
	control.OnAppStatus = func(mode int, err error) { modes = append(modes, mode) }

	control.SendAppStatus(AppStatusMirrorLive)
	control.SendAppStatus(AppStatusMirrorLive)
	control.SendAppStatus(AppStatusBackground)

	if len(modes) != 2 || modes[0] != AppStatusMirrorLive || modes[1] != AppStatusBackground {
		t.Fatalf("modes reported = %v, want one of each", modes)
	}
}

// decodeNumbers pulls the numeric fields out of a body that also carries booleans.
func decodeNumbers(t *testing.T, body []byte) map[string]float64 {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	out := map[string]float64{}
	for key, value := range raw {
		if number, ok := value.(float64); ok {
			out[key] = number
		}
	}
	return out
}
