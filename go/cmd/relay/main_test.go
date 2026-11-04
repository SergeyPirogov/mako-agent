package main

import (
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/relay"
)

func TestLimitsFromEnv(t *testing.T) {
	env := map[string]string{"RELAY_MAX_MACS": "50", "RELAY_RATE": "2.5", "RELAY_MAX_BODY": "1024"}
	l, err := limitsFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	d := relay.DefaultLimits()
	if l.MaxMacs != 50 || l.Rate != 2.5 || l.MaxBody != 1024 || l.MacsPerIP != d.MacsPerIP || l.Burst != d.Burst {
		t.Fatalf("%+v", l)
	}
	for _, bad := range []map[string]string{{"RELAY_BURST": "-1"}, {"RELAY_RATE": "fast"}, {"RELAY_MAX_BODY": "0"}} {
		if _, err := limitsFromEnv(func(k string) string { return bad[k] }); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}
