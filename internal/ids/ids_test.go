package ids_test

import (
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/client/internal/ids"
)

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestStintIdsSortByTimeAndKeysNeverRepeat(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	t0 := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	a := ids.Stint(t0)
	b := ids.Stint(t0.Add(time.Second))
	r.Regexp(uuid, a)
	r.Equal("7", a[14:15], "version 7")
	r.Contains("89ab", a[19:20], "the RFC variant")
	r.Less(a, b, "later stints sort later")
	r.Equal(a[:8], ids.Stint(t0)[:8], "the same millisecond shares its prefix")

	seen := map[string]bool{}
	for range 1000 {
		k := ids.Key()
		r.Regexp(uuid, k)
		r.Equal("4", k[14:15], "version 4")
		r.False(seen[k])
		seen[k] = true
	}
}
