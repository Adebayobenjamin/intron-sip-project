// Package ids generates call identifiers.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Generator makes call UUIDs.
//
// A UUIDv4, chosen by us BEFORE the call exists. That is the point: the socket
// FreeSWITCH opens when the far end answers carries this value as
// origination_uuid, so it is matched deterministically rather than by
// correlating on caller and callee — which breaks the first time two calls to
// the same number overlap.
type Generator struct{}

// New returns a random UUIDv4.
func (Generator) New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a condition to paper over with a
		// timestamp: a predictable call UUID is a channel someone else can
		// address.
		panic("ids: crypto/rand unavailable: " + err.Error())
	}

	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}
