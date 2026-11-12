// Package dailywindow describes a span of time that recurs every day, such as
// a nightly maintenance window, in a named time zone.
package dailywindow

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrInvalid is returned for a window that cannot be parsed.
var ErrInvalid = errors.New("invalid daily window")

// Window is a daily span from Start (inclusive) to End (exclusive), local to
// its time zone. End before Start means the window crosses midnight, e.g.
// "22:00-02:00".
type Window struct {
	start, end time.Duration // since local midnight
	loc        *time.Location
	spec       string
}

// Parse parses spec, written "HH:MM-HH:MM" in 24-hour time, as a window in
// the IANA time zone tz, e.g. "Asia/Taipei". The two times must differ.
func Parse(spec, tz string) (Window, error) {
	from, to, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return Window{}, fmt.Errorf("%w %q: want HH:MM-HH:MM", ErrInvalid, spec)
	}
	start, err := parseClock(from)
	if err != nil {
		return Window{}, fmt.Errorf("%w %q: start: %v", ErrInvalid, spec, err)
	}
	end, err := parseClock(to)
	if err != nil {
		return Window{}, fmt.Errorf("%w %q: end: %v", ErrInvalid, spec, err)
	}
	if start == end {
		return Window{}, fmt.Errorf("%w %q: start and end are the same", ErrInvalid, spec)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "" {
		return Window{}, fmt.Errorf("%w: unknown time zone %q", ErrInvalid, tz)
	}
	return Window{start: start, end: end, loc: loc, spec: strings.TrimSpace(spec)}, nil
}

// parseClock parses "HH:MM" into the time since midnight.
func parseClock(s string) (time.Duration, error) {
	hh, mm, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || len(hh) != 2 || len(mm) != 2 {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	h, errH := strconv.Atoi(hh)
	m, errM := strconv.Atoi(mm)
	if errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q is not a time of day", s)
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, nil
}

// Contains reports whether t falls inside the window, judged by the wall
// clock in the window's time zone.
func (w Window) Contains(t time.Time) bool {
	local := t.In(w.loc)
	since := time.Duration(local.Hour())*time.Hour + time.Duration(local.Minute())*time.Minute +
		time.Duration(local.Second())*time.Second + time.Duration(local.Nanosecond())
	if w.start < w.end {
		return since >= w.start && since < w.end
	}
	return since >= w.start || since < w.end
}

// Describe renders the window with its UTC equivalent on the day of ref, e.g.
// "02:00-05:00 Asia/Taipei (18:00-21:00 UTC)", so whoever reads a log can
// check it against either clock. The UTC times are those of ref's day, which
// matters only for a time zone that observes daylight saving.
func (w Window) Describe(ref time.Time) string {
	day := ref.In(w.loc)
	midnight := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, w.loc)
	from := midnight.Add(w.start).UTC().Format("15:04")
	to := midnight.Add(w.end).UTC().Format("15:04")
	return fmt.Sprintf("%s %s (%s-%s UTC)", w.spec, w.loc, from, to)
}
