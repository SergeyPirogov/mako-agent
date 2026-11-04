package relay

import (
	"sync"
	"time"
)

// Limits keep a shared relay up under public use: one Mac, phone or address cannot take it all.
type Limits struct {
	MaxMacs    int           // Macs online at once
	MacsPerIP  int           // Mac tunnels from one address
	ConnsPerIP int           // a phone's requests and WebSockets in flight from one address
	Rate       float64       // requests a second to one Mac, on average
	Burst      int           // requests to one Mac at once before Rate holds them back
	MaxPending int           // requests and WebSockets in flight to one Mac
	MaxBody    int64         // a request's or answer's body
	MaxMessage int           // one WebSocket message of a phone
	Ping       time.Duration // how often the relay asks the Mac whether the tunnel is alive
	Idle       time.Duration // a tunnel with nothing from the Mac for this long is closed
	StreamIdle time.Duration // a phone's WebSocket with no message either way for this long is closed
	Response   time.Duration // how long a phone waits for the Mac's answer
}

// DefaultLimits: what the shared relay runs with.
func DefaultLimits() Limits {
	return Limits{
		MaxMacs: 1000, MacsPerIP: 4, ConnsPerIP: 64, Rate: 20, Burst: 60, MaxPending: 128,
		MaxBody: 16 << 20, MaxMessage: 1 << 20,
		Ping: 25 * time.Second, Idle: 90 * time.Second, StreamIdle: 10 * time.Minute, Response: 60 * time.Second,
	}
}

// orDefault: the limits with every unset one at its default.
func (l Limits) orDefault() Limits {
	d := DefaultLimits()
	set := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	setD := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	set(&l.MaxMacs, d.MaxMacs)
	set(&l.MacsPerIP, d.MacsPerIP)
	set(&l.ConnsPerIP, d.ConnsPerIP)
	set(&l.Burst, d.Burst)
	set(&l.MaxPending, d.MaxPending)
	set(&l.MaxMessage, d.MaxMessage)
	if l.Rate <= 0 {
		l.Rate = d.Rate
	}
	if l.MaxBody <= 0 {
		l.MaxBody = d.MaxBody
	}
	setD(&l.Ping, d.Ping)
	setD(&l.Idle, d.Idle)
	setD(&l.StreamIdle, d.StreamIdle)
	setD(&l.Response, d.Response)
	return l
}

// maxFrame: the largest tunnel message, a body of MaxBody in base64 (JSON) with room for the rest.
func (l Limits) maxFrame() int { return int(l.MaxBody*4/3) + 64<<10 }

// counter counts what is open per key (an address) and forgets a key at zero.
type counter struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *counter) acquire(key string, max int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int{}
	}
	if c.m[key] >= max {
		return false
	}
	c.m[key]++
	return true
}

func (c *counter) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[key]--; c.m[key] <= 0 {
		delete(c.m, key)
	}
}

func (c *counter) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// rater is a token bucket per key (a Mac). A bucket that has filled up again is the same as none, so it is
// dropped: the map holds only the Macs that were busy lately.
type rater struct {
	mu    sync.Mutex
	rate  float64
	burst float64
	m     map[string]*bucket
	swept time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func (r *rater) allow(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]*bucket{}
	}
	if now.Sub(r.swept) > time.Minute {
		r.sweepLocked(now)
	}
	b := r.m[key]
	if b == nil {
		b = &bucket{tokens: r.burst, last: now}
		r.m[key] = b
	}
	b.tokens = min(r.burst, b.tokens+now.Sub(b.last).Seconds()*r.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (r *rater) sweepLocked(now time.Time) {
	r.swept = now
	for k, b := range r.m {
		if b.tokens+now.Sub(b.last).Seconds()*r.rate >= r.burst {
			delete(r.m, k)
		}
	}
}

// len: the buckets that have not filled up by now.
func (r *rater) len(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
	return len(r.m)
}
