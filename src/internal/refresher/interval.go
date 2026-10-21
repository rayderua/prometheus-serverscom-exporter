package refresher

import (
	"errors"
	"time"
)

// Interval is a refresh interval: a fixed duration, or auto, which shares
// the request budget left after the fixed intervals among auto collectors.
// It implements kingpin.Value.
type Interval struct {
	Auto  bool
	Fixed time.Duration
}

// AutoInterval is the auto interval.
var AutoInterval = Interval{Auto: true}

// Set parses "auto" or a positive duration.
func (i *Interval) Set(s string) error {
	if s == "auto" {
		*i = AutoInterval
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return errors.New(`want "auto" or a duration, e.g. 15m`)
	}
	if d <= 0 {
		return errors.New("must be positive; disable a collector with --no-collector.<name>")
	}
	*i = Interval{Fixed: d}
	return nil
}

func (i Interval) String() string {
	if i.Auto {
		return "auto"
	}
	return formatDuration(i.Fixed)
}
