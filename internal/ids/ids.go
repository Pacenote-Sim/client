// Package ids mints the identifiers the client sends: a UUIDv7 for a stint, so
// that stints sort by time and a stint created offline has an id before the
// server hears of it; a random UUIDv4 for an idempotency key, so that a retry
// of the same write carries the same key and a new write never does.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// Stint is a UUIDv7 for the given moment: 48 bits of Unix milliseconds, then
// random bits, with the version and variant set as RFC 9562 says.
func Stint(now time.Time) string {
	var b [16]byte
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(max(now.UnixMilli(), 0)))
	copy(b[:6], ms[2:])
	fill(b[6:])
	b[6] = 0x70 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f
	return format(b)
}

// Key is a random UUIDv4, for an Idempotency-Key.
func Key() string {
	var b [16]byte
	fill(b[:])
	b[6] = 0x40 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f
	return format(b)
}

// fill is crypto/rand, which since Go 1.24 always fills and never errors.
func fill(b []byte) { _, _ = rand.Read(b) }

func format(b [16]byte) string {
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}
