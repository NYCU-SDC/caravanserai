package controller

import "time"

// Clock abstracts time operations so that controllers can be tested with a
// deterministic fake clock instead of relying on the real wall clock.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// Since returns the time elapsed since t.
	Since(t time.Time) time.Duration
}

// realClock delegates to the standard time package.
type realClock struct{}

func (realClock) Now() time.Time                  { return time.Now() }
func (realClock) Since(t time.Time) time.Duration { return time.Since(t) }

// options holds shared configuration that can be applied to any controller
// that supports functional options.
type options struct {
	clock        Clock
	seedInterval time.Duration
}

// Option configures optional behaviour on controllers that accept it.
type Option func(*options)

// WithClock overrides the default wall clock.  This is intended for tests
// that need deterministic time control.
func WithClock(c Clock) Option {
	return func(o *options) {
		o.clock = c
	}
}

// WithSeedInterval overrides the default seed/resync ticker interval.  This is
// intended for integration tests that need faster convergence.
func WithSeedInterval(d time.Duration) Option {
	return func(o *options) {
		o.seedInterval = d
	}
}

// applyOptions folds all Option functions into an options struct with sensible
// defaults.  Controllers should call this once in their constructor and read
// whichever fields they need.  seedInterval is left at zero — each controller
// must check and apply its own default if o.seedInterval == 0.
func applyOptions(opts []Option) options {
	o := options{}
	for _, fn := range opts {
		fn(&o)
	}
	if o.clock == nil {
		o.clock = realClock{}
	}
	return o
}
