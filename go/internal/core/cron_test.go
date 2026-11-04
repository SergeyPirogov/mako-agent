package core

import (
	"testing"
	"time"
)

func TestCronNext(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct{ spec, after, want string }{
		{"0 9 * * 1", "2026-11-04 10:00", "2026-11-09 09:00"}, // Wednesday → next Monday
		{"0 9 * * 1", "2026-11-09 08:59", "2026-11-09 09:00"},
		{"0 9 * * 1", "2026-11-09 09:00", "2026-11-16 09:00"}, // strictly after
		{"*/15 * * * *", "2026-11-04 10:07", "2026-11-04 10:15"},
		{"30 8-18/2 * * *", "2026-11-04 18:31", "2026-11-05 08:30"},
		{"0 0 1 * *", "2026-12-15 00:00", "2027-01-01 00:00"},
		{"0 12 * * 0", "2026-11-04 00:00", "2026-11-08 12:00"},
		{"0 12 * * 7", "2026-11-04 00:00", "2026-11-08 12:00"},   // 7 is Sunday too
		{"0 6 1,15 * 5", "2026-11-04 00:00", "2026-11-06 06:00"}, // the 1st, the 15th or a Friday
		{"5 4 * 2 *", "2026-11-04 00:00", "2027-02-01 04:05"},
	}
	for _, c := range cases {
		cr, err := ParseCron(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		if got := cr.Next(at(c.after)); !got.Equal(at(c.want)) {
			t.Errorf("%s after %s: %s, want %s", c.spec, c.after, got.Format("2006-01-02 15:04"), c.want)
		}
	}
	if cr, _ := ParseCron("0 0 31 2 *"); !cr.Next(at("2026-01-01 00:00")).IsZero() {
		t.Error("a date that never comes has no next time")
	}
}

func TestParseCronRejects(t *testing.T) {
	for _, s := range []string{"", "0 9 * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "*/0 * * * *", "a * * * *", "5-1 * * * *", "0 9 * * 1 2"} {
		if _, err := ParseCron(s); err == nil {
			t.Errorf("%q: no error", s)
		}
	}
	if c, err := ParseCron(" 0  9 * * 1 "); err != nil || c.Spec != "0 9 * * 1" {
		t.Errorf("%+v %v", c, err)
	}
}
