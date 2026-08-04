package net

import (
	"encoding/json"
	"fmt"
	"time"
)

// The dash asks the phone what time it is with PXC command 0x10450 as well as
// with 0x10600, and the two want different answers: 0x10600 wants the binary
// stamp in hutimesync.go, 0x10450 wants JSON. Which one a dash uses is
// firmware-specific and they do not overlap - a rider's Voge log
// (DIRECT-VOGE-034672, modelId 37504, 2026-08-02) carries exactly one 0x10450,
// at RX #8 right after the handshake with an empty body, and not a single
// 0x10600 across five days. On such a dash the generic "ack any even command
// with an empty cmd+1" branch answered the only clock question ever asked with
// no clock in it, so the dash was never told the time at all - which is what
// riders saw as the dash losing its clock once the app went away.
//
// Field names, types and the two date layouts below are those the official
// Carbit Ride app sends (decompiled 2026-08-04, handler ih/n0.java).
const (
	queryTimeDateLayout = "02.01.2006 15:04:05"
	// Two head-unit models want the date separated by colons as well. Carbit
	// selects on the head unit's own model id, which arrives in HUD_CONFIG as
	// "channel" - the same number the pairing QR calls modelId.
	queryTimeColonDateLayout = "02:01:2006 15:04:05"
	queryTimeColonDateModelA = "21312"
	queryTimeColonDateModelB = "21313"
)

type queryTimeReply struct {
	// Time is plain epoch milliseconds.
	Time int64 `json:"time"`
	// CurrentTime is epoch milliseconds shifted by the zone offset, i.e. local
	// wall-clock read as if it were UTC. Carbit sends both, so both are sent.
	CurrentTime int64 `json:"currentTime"`
	// CurrentTimeZone is an IANA zone id such as "Europe/Rome".
	CurrentTimeZone string `json:"currentTimeZone"`
	// DateTime is omitted unless the dash asked for it by advertising
	// supportSyncCorrectTime, matching Carbit. A dash that did not ask for a
	// preformatted date should not start receiving one because of us.
	DateTime string `json:"dateTime,omitempty"`
}

// queryTimeAck builds the JSON body for a 0x10451 reply.
//
// conf may be nil: the reply is due as soon as the dash asks, and nothing here
// needs the handshake to have completed. zoneID is the host's own zone id and
// wins when set, because Android knows it authoritatively and Go's local
// location is usually just named "Local" on a device.
func queryTimeAck(now time.Time, zoneID string, conf *HUDConfig) []byte {
	_, offsetSeconds := now.Zone()
	reply := queryTimeReply{
		Time:            now.UnixMilli(),
		CurrentTime:     now.UnixMilli() + int64(offsetSeconds)*1000,
		CurrentTimeZone: queryTimeZoneID(now, zoneID),
	}
	if conf != nil && conf.SupportSyncCorrectTime {
		reply.DateTime = queryTimeDateTime(now, conf.Channel)
	}
	body, err := json.Marshal(reply)
	if err != nil {
		// Unreachable for these field types. Returning nil degrades to the empty
		// cmd+1 this command used to get, which is the behaviour every dash in
		// the field already survives - never to sending nothing at all.
		return nil
	}
	return body
}

// queryTimeZoneID prefers the id the host supplied and otherwise makes the best
// of what Go knows. A device whose local location has no real name falls back to
// a fixed-offset id rather than the literal "Local", which no dash could parse.
func queryTimeZoneID(now time.Time, configured string) string {
	if configured != "" {
		return configured
	}
	if name := now.Location().String(); name != "" && name != "Local" {
		return name
	}
	_, offsetSeconds := now.Zone()
	sign := "+"
	if offsetSeconds < 0 {
		sign = "-"
		offsetSeconds = -offsetSeconds
	}
	return fmt.Sprintf("GMT%s%02d:%02d", sign, offsetSeconds/3600, (offsetSeconds%3600)/60)
}

// queryTimeDateTime renders the "dateTime" string. Go layouts cannot express a
// colon before the fractional second, so the milliseconds are appended by hand.
func queryTimeDateTime(now time.Time, huModel string) string {
	layout := queryTimeDateLayout
	if huModel == queryTimeColonDateModelA || huModel == queryTimeColonDateModelB {
		layout = queryTimeColonDateLayout
	}
	return fmt.Sprintf("%s:%03d", now.Format(layout), now.Nanosecond()/int(time.Millisecond))
}
