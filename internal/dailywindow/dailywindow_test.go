package dailywindow

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	valid := []string{"02:00-05:00", "22:00-02:00", " 00:00-23:59 ", "13:30-13:31"}
	for _, spec := range valid {
		_, err := Parse(spec, "Asia/Taipei")
		assert.NoError(t, err, spec)
	}

	invalid := []string{"", "02:00", "2:00-05:00", "02:00-5:00", "24:00-05:00", "02:60-05:00", "02:00-02:00", "aa:bb-cc:dd", "02:00~05:00"}
	for _, spec := range invalid {
		_, err := Parse(spec, "Asia/Taipei")
		assert.ErrorIs(t, err, ErrInvalid, spec)
	}

	_, err := Parse("02:00-05:00", "Mars/Olympus")
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = Parse("02:00-05:00", "")
	assert.ErrorIs(t, err, ErrInvalid, "an empty time zone would silently mean UTC")
}

func TestContains_TaiwanTime(t *testing.T) {
	w, err := Parse("02:00-05:00", "Asia/Taipei")
	require.NoError(t, err)

	utc := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		require.NoError(t, err)
		return tm
	}
	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		// Taiwan is UTC+8 all year: 02:00-05:00 there is 18:00-21:00 UTC the
		// day before.
		{"02:30 Taiwan is 18:30 UTC the day before", utc("2026-10-09T18:30:00Z"), true},
		{"02:00 Taiwan, the start, is inside", utc("2026-10-09T18:00:00Z"), true},
		{"04:59:59 Taiwan is inside", utc("2026-10-09T20:59:59Z"), true},
		{"05:00 Taiwan, the end, is outside", utc("2026-10-09T21:00:00Z"), false},
		{"01:59 Taiwan is outside", utc("2026-10-09T17:59:00Z"), false},
		{"02:30 UTC is 10:30 Taiwan, outside", utc("2026-10-10T02:30:00Z"), false},
		{"the same instant in another zone gives the same answer", utc("2026-10-09T18:30:00Z").In(time.FixedZone("X", -5*3600)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, w.Contains(tt.at))
		})
	}
}

func TestContains_AcrossMidnight(t *testing.T) {
	w, err := Parse("22:00-02:00", "Asia/Taipei")
	require.NoError(t, err)
	tw := time.FixedZone("UTC+8", 8*3600)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 10, h, m, 0, 0, tw) }

	assert.True(t, w.Contains(at(22, 0)))
	assert.True(t, w.Contains(at(23, 59)))
	assert.True(t, w.Contains(at(0, 0)))
	assert.True(t, w.Contains(at(1, 59)))
	assert.False(t, w.Contains(at(2, 0)))
	assert.False(t, w.Contains(at(12, 0)))
	assert.False(t, w.Contains(at(21, 59)))
}

func TestDescribe(t *testing.T) {
	w, err := Parse("02:00-05:00", "Asia/Taipei")
	require.NoError(t, err)
	assert.Equal(t, "02:00-05:00 Asia/Taipei (18:00-21:00 UTC)", w.Describe(time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)))
}
