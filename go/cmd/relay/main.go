// relay: the shared server between a phone and its Mac (docs/relay.md). Each Mac keeps a tunnel to it, and a phone
// opening https://<id>.$RELAY_DOMAIN reaches the Mac whose token gives that id. TLS ends on the platform in front.
//
//	RELAY_DOMAIN     the relay's domain, "relay.example.com" (required)
//	PORT             where to listen, 8080
//	RELAY_IP_HEADER  the header the platform puts the client's address in ("Fly-Client-IP"); empty: the socket's
//	RELAY_METRICS_PORT  serve /_relay/metrics on this port only, not on PORT (the platform exposes PORT to the
//	                 internet, not this one); empty: on PORT too, for a relay run by hand
//	RELAY_MAX_MACS, RELAY_MACS_PER_IP, RELAY_CONNS_PER_IP, RELAY_RATE, RELAY_BURST, RELAY_MAX_PENDING,
//	RELAY_MAX_BODY   the limits (relay.DefaultLimits when unset)
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/relay"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	domain := os.Getenv("RELAY_DOMAIN")
	if domain == "" {
		return fmt.Errorf("RELAY_DOMAIN is not set: the relay's domain, such as relay.example.com")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	limits, err := limitsFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	metricsPort := os.Getenv("RELAY_METRICS_PORT")
	rs := &relay.Server{Domain: domain, Limits: limits, IPHeader: os.Getenv("RELAY_IP_HEADER"), HideMetrics: metricsPort != ""}
	srv := &http.Server{Addr: ":" + port, Handler: rs, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("relay: *.%s on :%s", domain, port)
	var msrv *http.Server
	if metricsPort != "" {
		mux := http.NewServeMux()
		mux.Handle("/_relay/metrics", rs.Metrics())
		msrv = &http.Server{Addr: ":" + metricsPort, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- msrv.ListenAndServe() }()
		log.Printf("relay: metrics on :%s", metricsPort)
		defer msrv.Close()
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// The Macs reconnect to the new machine by themselves; the phones' requests in flight get to finish.
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// limitsFromEnv: relay.DefaultLimits with what RELAY_* sets over them.
func limitsFromEnv(getenv func(string) string) (relay.Limits, error) {
	l := relay.DefaultLimits()
	ints := map[string]*int{
		"RELAY_MAX_MACS": &l.MaxMacs, "RELAY_MACS_PER_IP": &l.MacsPerIP, "RELAY_CONNS_PER_IP": &l.ConnsPerIP,
		"RELAY_BURST": &l.Burst, "RELAY_MAX_PENDING": &l.MaxPending,
	}
	for name, v := range ints {
		if s := getenv(name); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n <= 0 {
				return l, fmt.Errorf("%s=%q: a positive number is needed", name, s)
			}
			*v = n
		}
	}
	if s := getenv("RELAY_RATE"); s != "" {
		r, err := strconv.ParseFloat(s, 64)
		if err != nil || r <= 0 {
			return l, fmt.Errorf("RELAY_RATE=%q: requests a second, a positive number", s)
		}
		l.Rate = r
	}
	if s := getenv("RELAY_MAX_BODY"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return l, fmt.Errorf("RELAY_MAX_BODY=%q: bytes, a positive number", s)
		}
		l.MaxBody = n
	}
	return l, nil
}
