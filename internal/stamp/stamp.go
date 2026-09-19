// Package stamp reads the server address the team's server wrote into this
// executable.
//
// A client is built by the server and downloaded by a driver, who should never
// have to type a server address. So the executable carries a reserved region
// of [RegionSize] bytes in its data section, and the server writes the address
// into that region before offering the file. This package is the reading half
// of a format whose writing half is the server's clientbuild package; the two
// are separate programs in separate repositories, so every offset here is a
// contract and not an implementation detail. Changing a number here means
// changing it there and giving the format a new version.
//
// The region:
//
//	[0:16]      head marker
//	[16]        format version — 0 in a prebuilt binary, 1 once stamped
//	[17]        reserved, zero
//	[18:20]     payload length, big-endian uint16
//	[20:24]     CRC-32 (IEEE) of the payload, big-endian
//	[24:1008]   the payload, [PayloadMax] bytes of room
//	[1008:1024] tail marker
//
// The payload is key=value lines. A key this client does not know is read
// past, so a newer server can say more without breaking an older client.
package stamp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
)

// The shape of the region, byte for byte the server's.
const (
	RegionSize = 1024
	MarkerSize = 16
	HeaderSize = 8
	PayloadMax = RegionSize - 2*MarkerSize - HeaderSize

	offVersion  = MarkerSize
	offReserved = offVersion + 1
	offLength   = offReserved + 1
	offChecksum = offLength + 2
	offPayload  = MarkerSize + HeaderSize
	offTail     = RegionSize - MarkerSize

	// VersionBlank is a region nothing has been written into; Version1 the
	// key=value payload this client reads.
	VersionBlank = 0
	Version1     = 1

	// KeyAddress is the server address; KeyBuild the build that wrote the stamp.
	KeyAddress = "url"
	KeyBuild   = "build"
)

// The markers, assembled from halves so that the whole marker appears in this
// binary exactly once: in the region, and nowhere in the code that reads it.
var (
	markerPrefix = "PACENOTE"
	headMarker   = []byte(markerPrefix + "STAMP_HD")
	tailMarker   = []byte(markerPrefix + "STAMP_TL")
)

// The errors a caller acts on.
var (
	// ErrBlank reports a region that was never stamped: this is a prebuilt
	// client that did not come from a server. The app asks for an address.
	ErrBlank = errors.New("stamp: this client was not built by a server and carries no address")
	// ErrCorrupt reports a region that does not check out. The app treats it
	// as blank and says so in the log.
	ErrCorrupt = errors.New("stamp: the address written into this client is not readable")
)

// Stamp is what the server wrote.
type Stamp struct {
	// Address is the server: a scheme and a host, no trailing slash.
	Address string
	// Build identifies the server's build of this file, or is empty.
	Build string
}

// Read reads the stamp out of this executable's own region.
func Read() (Stamp, error) {
	return ReadRegion(region[:])
}

// ReadRegion parses one region. It is the same check the server makes before
// offering a file, so the two never disagree about what a file says.
func ReadRegion(r []byte) (Stamp, error) {
	if len(r) != RegionSize {
		return Stamp{}, fmt.Errorf("%w: the region is %d bytes rather than %d", ErrCorrupt, len(r), RegionSize)
	}
	if !bytes.Equal(r[:MarkerSize], headMarker) || !bytes.Equal(r[offTail:], tailMarker) {
		return Stamp{}, fmt.Errorf("%w: the markers are not where they should be", ErrCorrupt)
	}
	switch v := r[offVersion]; v {
	case VersionBlank:
		return Stamp{}, ErrBlank
	case Version1:
	default:
		return Stamp{}, fmt.Errorf("%w: it is format version %d and this client reads %d", ErrCorrupt, v, Version1)
	}
	n := int(binary.BigEndian.Uint16(r[offLength:offChecksum]))
	if n > PayloadMax {
		return Stamp{}, fmt.Errorf("%w: it claims %d bytes and the region holds %d", ErrCorrupt, n, PayloadMax)
	}
	payload := r[offPayload : offPayload+n]
	if want, got := binary.BigEndian.Uint32(r[offChecksum:offPayload]), crc32.ChecksumIEEE(payload); want != got {
		return Stamp{}, fmt.Errorf("%w: the checksum does not match", ErrCorrupt)
	}
	s, err := parse(payload)
	if err != nil {
		return Stamp{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	return s, nil
}

// parse reads key=value lines. Unknown keys are read past; a line without "="
// or a key given twice is an error, since the writer never produces either.
func parse(payload []byte) (Stamp, error) {
	var s Stamp
	seen := map[string]bool{}
	for i, line := range strings.Split(string(payload), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Stamp{}, fmt.Errorf("line %d has no key", i+1)
		}
		if seen[key] {
			return Stamp{}, fmt.Errorf("%q is given twice", key)
		}
		seen[key] = true
		switch key {
		case KeyAddress:
			s.Address = value
		case KeyBuild:
			s.Build = value
		}
	}
	if s.Address == "" {
		return Stamp{}, errors.New("no server address in it")
	}
	return s, nil
}
