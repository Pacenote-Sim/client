package stamp_test

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/client/internal/stamp"
)

// write is the server's writer, re-spelled here so that the test does not
// depend on the server: a region with a version-1 payload.
func write(payload string) []byte {
	r := blank()
	r[stamp.MarkerSize] = stamp.Version1
	binary.BigEndian.PutUint16(r[18:20], uint16(len(payload)))
	binary.BigEndian.PutUint32(r[20:24], crc32.ChecksumIEEE([]byte(payload)))
	copy(r[24:], payload)
	return r
}

func blank() []byte {
	r := make([]byte, stamp.RegionSize)
	copy(r, "PACENOTE"+"STAMP_HD")
	copy(r[stamp.RegionSize-stamp.MarkerSize:], "PACENOTE"+"STAMP_TL")
	return r
}

func TestAStampReadsBackWhatTheServerWrote(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	s, err := stamp.ReadRegion(write("url=https://team.example\nbuild=b-42"))
	r.NoError(err)
	r.Equal(stamp.Stamp{Address: "https://team.example", Build: "b-42"}, s)

	s, err = stamp.ReadRegion(write("url=http://localhost:8080"))
	r.NoError(err)
	r.Equal(stamp.Stamp{Address: "http://localhost:8080"}, s)

	s, err = stamp.ReadRegion(write("url=https://team.example\ncolour=blue\n"))
	r.NoError(err, "a key this client does not know is read past")
	r.Equal("https://team.example", s.Address)
}

func TestWhatIsNotAStamp(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	_, err := stamp.ReadRegion(blank())
	r.ErrorIs(err, stamp.ErrBlank, "a prebuilt client")

	for name, region := range map[string][]byte{
		"the wrong size":       make([]byte, 100),
		"no markers":           make([]byte, stamp.RegionSize),
		"a version from later": func() []byte { b := write("url=x"); b[16] = 7; return b }(),
		"a length past the room": func() []byte {
			b := write("url=x")
			binary.BigEndian.PutUint16(b[18:20], stamp.PayloadMax+1)
			return b
		}(),
		"a bad checksum":       func() []byte { b := write("url=https://a"); b[30] ^= 1; return b }(),
		"a line without a key": write("https://team.example"),
		"a key given twice":    write("url=a\nurl=b"),
		"no address":           write("build=b-1"),
	} {
		_, err := stamp.ReadRegion(region)
		r.ErrorIs(err, stamp.ErrCorrupt, name)
	}
}

// This executable carries the region itself, once, blank: what the server
// finds when it stamps a prebuilt client. The test builds the real command
// and scans the file the way the server does.
func TestTheBinaryCarriesExactlyOneBlankRegion(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	s, err := stamp.Read()
	r.ErrorIs(err, stamp.ErrBlank, "the test binary was never stamped")
	r.Empty(s.Address)

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	exe := filepath.Join(t.TempDir(), "pacenote")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command(goBin, "build", "-trimpath", "-o", exe, "../../cmd/pacenote")
	out, err := build.CombinedOutput()
	r.NoError(err, string(out))
	file, err := os.ReadFile(exe)
	r.NoError(err)

	head, tail := []byte("PACENOTE"+"STAMP_HD"), []byte("PACENOTE"+"STAMP_TL")
	found := 0
	for at := 0; ; {
		i := bytes.Index(file[at:], head)
		if i < 0 {
			break
		}
		start := at + i
		at = start + 1
		if start+stamp.RegionSize <= len(file) && bytes.Equal(file[start+stamp.RegionSize-stamp.MarkerSize:start+stamp.RegionSize], tail) {
			found++
			region, err := stamp.ReadRegion(file[start : start+stamp.RegionSize])
			r.ErrorIs(err, stamp.ErrBlank)
			r.Empty(region.Address)
		}
	}
	r.Equal(1, found, "the server refuses a file with no region or with two")
}
