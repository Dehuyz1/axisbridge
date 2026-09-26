package ovo

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
)

// newUUID returns a random RFC-4122 v4 UUID (stdlib-only replacement for
// google/uuid — the OVO payload only needs a random hex-formatted id).
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// genFCMToken mirrors the Python push token filler: a syntactically valid
// but random FCM token. OVO never delivers push to it (polling is used).
func genFCMToken() string {
	r1 := make([]byte, 16)
	_, _ = rand.Read(r1)
	r2 := make([]byte, 60)
	_, _ = rand.Read(r2)
	enc := base64.RawStdEncoding
	return "FCM|" + enc.EncodeToString(r1) + ":APA91b" + enc.EncodeToString(r2)
}
