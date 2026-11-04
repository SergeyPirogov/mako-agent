package relay

import (
	"fmt"
	"io"
	"sync/atomic"
)

// Why the relay turned a phone or a Mac away, as relay_rejected_total{reason} counts them.
const (
	rejectOffline = "offline" // no tunnel from that Mac
	rejectRate    = "rate"    // too many requests to that Mac
	rejectIP      = "ip"      // too many connections from that address
	rejectMacs    = "macs"    // the relay holds MaxMacs already
	rejectBusy    = "busy"    // MaxPending in flight to that Mac
	rejectSize    = "size"    // a body, message or tunnel frame over its limit
	rejectTimeout = "timeout" // the Mac did not answer in time
	rejectGone    = "gone"    // the tunnel broke while the phone waited
)

var rejectReasons = []string{rejectOffline, rejectRate, rejectIP, rejectMacs, rejectBusy, rejectSize, rejectTimeout, rejectGone}

// metrics: counters with no Mac's id in them, served at /_relay/metrics in Prometheus' text format.
type metrics struct {
	phoneConns atomic.Int64
	requests   atomic.Int64
	bytesIn    atomic.Int64 // from phones
	bytesOut   atomic.Int64 // to phones
	rejected   map[string]*atomic.Int64
}

func newMetrics() *metrics {
	m := &metrics{rejected: map[string]*atomic.Int64{}}
	for _, r := range rejectReasons {
		m.rejected[r] = &atomic.Int64{}
	}
	return m
}

func (m *metrics) reject(reason string) { m.rejected[reason].Add(1) }

func (m *metrics) write(w io.Writer, macsOnline int) {
	gauge := func(name, help string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
	}
	count := func(name, help string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	gauge("relay_macs_online", "Macs with a tunnel open.", int64(macsOnline))
	gauge("relay_phone_conns", "Phone requests and WebSockets in flight.", m.phoneConns.Load())
	count("relay_requests_total", "Phone requests to a Mac's address.", m.requests.Load())
	fmt.Fprintf(w, "# HELP relay_rejected_total Requests and tunnels turned away, by reason.\n# TYPE relay_rejected_total counter\n")
	for _, r := range rejectReasons {
		fmt.Fprintf(w, "relay_rejected_total{reason=%q} %d\n", r, m.rejected[r].Load())
	}
	count("relay_bytes_in_total", "Body and message bytes from phones.", m.bytesIn.Load())
	count("relay_bytes_out_total", "Body and message bytes to phones.", m.bytesOut.Load())
}
