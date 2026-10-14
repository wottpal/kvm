// Package webguard holds the checks the device's web server runs on browser
// requests (FORK.md D15). A page on another site must not drive the device
// through the user's browser: neither by a cross-site request carrying the
// session cookie (CSRF, cross-site WebSocket hijacking), nor, while the device
// has no password, by a DNS name the attacker rebinds to the device's address.
// IsBrowser tells browsers from native clients, for the device's switch that
// turns browser access off (D16). It is plain Go, so it builds and tests on
// any host.
package webguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// Check decides whether the device answers r. passwordSet tells whether a
// password guards the device; names returns the other names it answers to
// (see LocalHost). It returns 0 to let r pass, or the status to reject it
// with and why.
func Check(r *http.Request, passwordSet bool, names func() []string) (int, error) {
	if !passwordSet && !LocalHost(r.Host, names) {
		return http.StatusForbidden, fmt.Errorf("%w: %q", ErrPublicHost, r.Host)
	}
	if NeedsOriginCheck(r) {
		if err := CrossOrigin(r); err != nil {
			return http.StatusForbidden, err
		}
	}
	return 0, nil
}

// ErrPublicHost rejects a request addressed to a name public DNS resolves
// while no password guards the device.
var ErrPublicHost = errors.New("public host name without a password")

// NeedsOriginCheck reports whether r can change state on the device: any
// method but GET, HEAD and OPTIONS, and a WebSocket handshake (signaling
// opens a session over a GET).
func NeedsOriginCheck(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return isWebSocketUpgrade(r)
	}
	return true
}

func isWebSocketUpgrade(r *http.Request) bool {
	for _, v := range r.Header.Values("Upgrade") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "websocket") {
				return true
			}
		}
	}
	return false
}

// CrossOrigin returns an error if r comes from a web page of another origin,
// with the rules of net/http's CrossOriginProtection (Go 1.25, which the
// module's go version does not allow yet). Browsers send Sec-Fetch-Site to
// potentially trustworthy URLs (HTTPS, localhost); it decides when present:
// same-origin or none (the user typed the URL) pass. Without it, Origin must
// name the host the request was sent to (scheme aside, as a proxy may speak
// plain HTTP to the device); "null" (a sandboxed frame, a local file) and an
// extension's or app's own scheme never do. Browsers send one or the other
// with every WebSocket handshake and every request that can change state,
// and no page can forge them. A request with neither comes from a client
// that is not a browser (curl, the BlitzKVM engine) and is not a cross-site
// vector.
func CrossOrigin(r *http.Request) error {
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "same-origin", "none":
		return nil
	case "":
	default:
		return fmt.Errorf("cross-origin request (Sec-Fetch-Site %q)", site)
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	if u, err := url.Parse(origin); err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host) {
		return nil
	}
	return fmt.Errorf("cross-origin request (Origin %q, Host %q)", origin, r.Host)
}

// privateSuffixes are DNS suffixes a rebinding page cannot get public DNS to
// resolve to an address of its choice: special-use names (RFC 6761, 6762,
// 8375, 9476: .localhost, .local, .home.arpa, .alt, .test, .invalid), names
// under top-level domains ICANN has not delegated and routers and companies
// use (.lan, .home, .internal, .localdomain, .intranet, .corp, .private), and
// Tailscale's ts.net: MagicDNS names resolve only in the tailnet, and Funnel
// names publicly only to Tailscale's own ingress.
var privateSuffixes = []string{
	"localhost", "local", "home.arpa", "alt", "test", "invalid",
	"lan", "home", "internal", "localdomain", "intranet", "corp", "private",
	"ts.net",
}

// LocalHost reports whether host (a Host header, with or without a port)
// names the device in a way a DNS rebinding attack cannot produce: an IP
// address, a name under a private suffix, or one of names: the device's own
// (single-label or under a router's domain) and those of a certificate its
// owner installed, where "*.example.com" stands for one label under
// example.com. names is called only for a host that is neither an IP address
// nor under a private suffix. Other single-label names are refused: a
// resolver may complete them with a search domain whose records others can
// write. An empty host (HTTP/1.0, never a browser) passes.
func LocalHost(host string, names func() []string) bool {
	if host == "" {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" {
		return false
	}
	// An IPv6 zone is percent-encoded in a Host header ("fe80::1%25eth0").
	if _, err := netip.ParseAddr(strings.Replace(host, "%25", "%", 1)); err == nil {
		return true
	}
	for _, suffix := range privateSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	if names == nil {
		return false
	}
	for _, name := range names() {
		if matchName(host, strings.TrimSuffix(strings.ToLower(name), ".")) {
			return true
		}
	}
	return false
}

// matchName reports whether host is name or, for a wildcard name, one label
// under its domain.
func matchName(host, name string) bool {
	if domain, ok := strings.CutPrefix(name, "*."); ok {
		label, rest, found := strings.Cut(host, ".")
		return found && label != "" && rest == domain
	}
	return name != "" && host == name
}

// IsBrowser reports whether r comes from a web browser (D16). Browsers send
// Origin or Sec-Fetch-Site with what a page sends, and every browser's
// User-Agent starts with "Mozilla/", which is all a navigation over plain
// HTTP carries. Native clients (the BlitzKVM engine, curl, URLSession) send
// none of them. Firefox lets a page set User-Agent on requests to its own
// origin, but no page can drop Origin, which every WebSocket handshake and
// POST carries: what starts a session or changes state is always caught.
func IsBrowser(r *http.Request) bool {
	return r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" ||
		strings.HasPrefix(r.Header.Get("User-Agent"), "Mozilla/")
}
