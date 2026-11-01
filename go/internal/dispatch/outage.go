package dispatch

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"

	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// A tracker that cannot be searched (no internet, the VPN dropped, the service is down, the token expired) is
// said on the board in words, with the tracker and its host, instead of a raw «dial tcp: lookup …».

// outage: why the search of t failed, in words: "GitLab · git.example.com: немає з'єднання — перевірте
// інтернет чи VPN".
func outage(t trackers.Tracker, err error) string {
	host := TrackerHost(t)
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil && u.Hostname() != "" {
			host = u.Hostname()
		}
	}
	who := t.Name()
	if host != "" {
		who += " · " + host
	}
	if login := t.Config().Assignee; badAssignee(err) && login != "" {
		return who + fmt.Sprintf(": user \"%s\" does not exist — fix \"Only assigned to\" in the settings", login)
	}
	return who + ": " + outageReason(err)
}

func outageReason(err error) string {
	reason, _ := classify(err)
	return reason
}

// classify: a failure of a tracker in words, and whether it is the network or the service (no internet or VPN, a
// dropped connection, a 429 or 5xx) rather than Mako or its settings. One place for both, so a new cause is not
// added to the words and forgotten in the verdict.
func classify(err error) (reason string, outside bool) {
	var he *trackers.HTTPError
	if errors.As(err, &he) {
		switch {
		case he.Status == 401 || he.Status == 403:
			return "access denied — check the token in the settings", false
		case he.Status == 429:
			return "too many requests, the service asks to wait", true
		case he.Status >= 500:
			return fmt.Sprintf("the service is down (%d), will try later", he.Status), true
		}
		return fmt.Sprintf("the service answered with error %d", he.Status), false
	}
	var dns *net.DNSError
	var cert x509.UnknownAuthorityError
	var host x509.HostnameError
	var ue *url.Error
	var oe *net.OpError
	switch {
	case errors.Is(err, context.Canceled):
		return "search interrupted", true
	case errors.As(err, &dns):
		return "server not found — check the internet or VPN", true
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the service refuses connections — it is down or needs a VPN", true
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETDOWN),
		errors.Is(err, syscall.EADDRNOTAVAIL), strings.Contains(err.Error(), "can't assign requested address"):
		return "network unreachable — check the internet or VPN", true
	case errors.Is(err, syscall.ECONNRESET):
		return "connection dropped — check the internet or VPN", true
	// Only the network's own timeouts: a deadline Mako set itself (an agent, a reviewer) is not the network.
	case errors.As(err, &ue) && ue.Timeout(), errors.As(err, &oe) && oe.Timeout():
		return "the service is not responding — check the internet or VPN", true
	case errors.As(err, &cert), errors.As(err, &host), strings.Contains(err.Error(), "x509:"):
		return "the server certificate was not accepted", false
	case errors.Is(err, context.DeadlineExceeded):
		return "the service is not responding — check the internet or VPN", false
	}
	return "search failed", false
}

// TrackerHost: the tracker's web host, "" when it cannot be told.
func TrackerHost(t trackers.Tracker) string {
	if ch, ok := t.(trackers.CodeHost); ok {
		return ch.Host()
	}
	if c := t.Config(); c != nil && c.BaseURL != "" {
		if u, err := url.Parse(c.BaseURL); err == nil {
			return u.Hostname()
		}
	}
	return ""
}

// badAssignee: the tracker refused the search for its assignee (GitHub: 422 on an unknown login).
func badAssignee(err error) bool {
	var he *trackers.HTTPError
	return errors.As(err, &he) && (he.Status == 422 || he.Status == 400) && strings.Contains(he.Body, `"assignee"`)
}

// transientErr: the failure comes from the network or the service, not from Mako, so it is only journaled.
func transientErr(err error) bool {
	_, outside := classify(err)
	return outside
}
