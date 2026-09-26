// Package axis holds AXIS-specific helpers. Only the phone helpers are ported
// so far; the HTTP client itself will land next.
package axis

import "strings"

// NormalizePhone converts any user input (with dashes, spaces, +62, 08, etc.)
// into the canonical 62xxx form. Empty on non-numeric junk.
func NormalizePhone(msisdn string) string {
	var b strings.Builder
	for _, r := range msisdn {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	clean := b.String()
	if clean == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(clean, "0"):
		return "62" + clean[1:]
	case !strings.HasPrefix(clean, "62"):
		return "62" + clean
	default:
		return clean
	}
}

// AxisPrefixes lists the 62-normalized MSISDN prefixes AXIS actually owns
// (0831/0832/0833/0838). Everything else the operator rejects with
// "Permintaan tidak lengkap" forever, so we filter at the door.
var AxisPrefixes = []string{"62831", "62832", "62833", "62838"}

// IsAxis reports whether a normalized (62xxx) MSISDN belongs to AXIS.
func IsAxis(msisdn62 string) bool {
	if len(msisdn62) < 10 || len(msisdn62) > 15 {
		return false
	}
	for _, p := range AxisPrefixes {
		if strings.HasPrefix(msisdn62, p) {
			return true
		}
	}
	return false
}

// ToLocal converts 62xxx to 0xxx for UI display and purchase payload.
func ToLocal(msisdn62 string) string {
	if strings.HasPrefix(msisdn62, "62") {
		return "0" + msisdn62[2:]
	}
	return msisdn62
}
