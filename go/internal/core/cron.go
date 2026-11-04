package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a five-field schedule ("0 9 * * 1": minute, hour, day of month, month, day of week), in local time.
// A field is "*", a number, a range "1-5", a list "1,3,5" and any of those with a step "*/15", "8-18/2".
// As in cron, when both the day of month and the day of week are restricted, either one matching is enough.
type Cron struct {
	Spec                          string
	minute, hour, dom, month, dow uint64 // bit i set: value i matches
	domAny, dowAny                bool
}

// ParseCron reads a five-field schedule; 7 in the day of week is Sunday, as 0.
func ParseCron(spec string) (Cron, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return Cron{}, fmt.Errorf("%q: expected five fields (minute hour day month weekday), for example \"0 9 * * 1\"", spec)
	}
	c := Cron{Spec: strings.Join(fields, " ")}
	var err error
	parts := []struct {
		out      *uint64
		min, max int
		name     string
	}{{&c.minute, 0, 59, "minute"}, {&c.hour, 0, 23, "hour"}, {&c.dom, 1, 31, "day"}, {&c.month, 1, 12, "month"}, {&c.dow, 0, 7, "weekday"}}
	for i, p := range parts {
		if *p.out, err = cronField(fields[i], p.min, p.max); err != nil {
			return Cron{}, fmt.Errorf("%q: %s: %w", spec, p.name, err)
		}
	}
	if c.dow&(1<<7) != 0 {
		c.dow |= 1 // 7 is Sunday too
	}
	c.domAny, c.dowAny = fields[2] == "*", fields[4] == "*"
	return c, nil
}

func cronField(f string, min, max int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(f, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepS)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad step %q", stepS)
			}
			step = n
		}
		lo, hi := min, max
		if rng != "*" {
			a, b, isRange := strings.Cut(rng, "-")
			x, err := strconv.Atoi(a)
			if err != nil {
				return 0, fmt.Errorf("bad value %q", part)
			}
			lo, hi = x, x
			if isRange {
				if hi, err = strconv.Atoi(b); err != nil {
					return 0, fmt.Errorf("bad value %q", part)
				}
			} else if hasStep {
				hi = max // "5/10": from 5 on, every 10
			}
		}
		if lo < min || hi > max || lo > hi {
			return 0, fmt.Errorf("%q is out of %d-%d", part, min, max)
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << v
		}
	}
	return bits, nil
}

func (c Cron) dayMatches(t time.Time) bool {
	dom, dow := c.dom&(1<<t.Day()) != 0, c.dow&(1<<int(t.Weekday())) != 0
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dow
	case c.dowAny:
		return dom
	}
	return dom || dow
}

// Next: the first minute after after that the schedule names; the zero time when there is none in five years
// ("0 0 31 2 *").
func (c Cron) Next(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	end := after.AddDate(5, 0, 0)
	for t.Before(end) {
		switch {
		case c.month&(1<<int(t.Month())) == 0:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
		case !c.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
		case c.hour&(1<<t.Hour()) == 0:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
		case c.minute&(1<<t.Minute()) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}
