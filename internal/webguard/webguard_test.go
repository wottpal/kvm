package webguard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func request(method, host string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(method, "http://"+host+"/webrtc/signaling/client", nil)
	r.Host = host
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestNeedsOriginCheck(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		headers map[string]string
		want    bool
	}{
		{"plain GET", http.MethodGet, nil, false},
		{"HEAD", http.MethodHead, nil, false},
		{"CORS preflight", http.MethodOptions, nil, false},
		{"WebSocket handshake", http.MethodGet, map[string]string{"Upgrade": "websocket"}, true},
		{"WebSocket token in a list", http.MethodGet, map[string]string{"Upgrade": "h2c, WebSocket"}, true},
		{"POST", http.MethodPost, nil, true},
		{"PUT", http.MethodPut, nil, true},
		{"DELETE", http.MethodDelete, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NeedsOriginCheck(request(tt.method, "192.168.1.50", tt.headers)); got != tt.want {
				t.Fatalf("NeedsOriginCheck = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCrossOrigin(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		headers map[string]string
		allowed bool
	}{
		// The device's own UI, over its IP, its name and HTTPS.
		{"same origin by IP", "192.168.1.50", map[string]string{"Origin": "http://192.168.1.50"}, true},
		{"same origin by name, case", "JetKVM.local", map[string]string{"Origin": "http://jetkvm.local"}, true},
		{"same origin over https", "kvm.example.com", map[string]string{"Origin": "https://kvm.example.com", "Sec-Fetch-Site": "same-origin"}, true},
		// Vite's dev proxy (changeOrigin) and a reverse proxy rewrite Host but
		// not Origin; the browser's Sec-Fetch-Site (localhost, HTTPS) decides.
		{"vite dev proxy", "192.168.1.50", map[string]string{"Origin": "http://localhost:5173", "Sec-Fetch-Site": "same-origin"}, true},
		{"https reverse proxy", "192.168.1.50", map[string]string{"Origin": "https://kvm.example.com", "Sec-Fetch-Site": "same-origin"}, true},
		{"https proxy to plain http", "kvm.example.com", map[string]string{"Origin": "https://kvm.example.com"}, true},
		// Clients that are not browsers send neither header.
		{"no origin (curl, BlitzKVM engine)", "192.168.1.50", nil, true},
		{"typed URL", "kvm.example.com", map[string]string{"Sec-Fetch-Site": "none"}, true},
		// Attacks.
		{"cross-site page over http", "192.168.1.50", map[string]string{"Origin": "http://evil.example"}, false},
		{"cross-site page over https", "kvm.example.com", map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, false},
		{"other port on the same address", "192.168.1.50", map[string]string{"Origin": "http://192.168.1.50:8080"}, false},
		{"lookalike host", "192.168.1.50", map[string]string{"Origin": "http://192.168.1.50.evil.example"}, false},
		{"opaque origin (sandboxed frame, local file)", "192.168.1.50", map[string]string{"Origin": "null"}, false},
		{"browser extension", "192.168.1.50", map[string]string{"Origin": "chrome-extension://abcdefghijklmnop"}, false},
		{"unreadable origin", "192.168.1.50", map[string]string{"Origin": "://"}, false},
		{"origin without host", "192.168.1.50", map[string]string{"Origin": "http://"}, false},
		{"cross-site without origin", "kvm.example.com", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"same-site is not same-origin", "kvm.example.com", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://other.example.com"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CrossOrigin(request(http.MethodPost, tt.host, tt.headers))
			if (err == nil) != tt.allowed {
				t.Fatalf("CrossOrigin = %v, want allowed %v", err, tt.allowed)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	names := func() []string { return []string{"jetkvm", "jetkvm.fritz.box"} }
	evil := map[string]string{"Origin": "http://evil.example"}
	tests := []struct {
		name        string
		method      string
		host        string
		headers     map[string]string
		passwordSet bool
		want        int
	}{
		{"read by IP, no password", http.MethodGet, "192.168.1.50", nil, false, 0},
		{"read by own name, no password", http.MethodGet, "jetkvm.fritz.box", nil, false, 0},
		{"rebinding name, no password", http.MethodGet, "rebind.evil.example", nil, false, http.StatusForbidden},
		{"rebinding name, password set", http.MethodGet, "kvm.example.com", nil, true, 0},
		{"cross-site POST, no password", http.MethodPost, "192.168.1.50", evil, false, http.StatusForbidden},
		{"cross-site POST, password set", http.MethodPost, "kvm.example.com", evil, true, http.StatusForbidden},
		{"cross-site WebSocket", http.MethodGet, "192.168.1.50", map[string]string{"Origin": "http://evil.example", "Upgrade": "websocket"}, true, http.StatusForbidden},
		{"cross-site plain GET (a link, an image)", http.MethodGet, "192.168.1.50", evil, true, 0},
		{"same-origin POST", http.MethodPost, "192.168.1.50", map[string]string{"Origin": "http://192.168.1.50"}, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Check(request(tt.method, tt.host, tt.headers), tt.passwordSet, names)
			if got != tt.want {
				t.Fatalf("Check = %d (%v), want %d", got, err, tt.want)
			}
		})
	}
}

func TestLocalHost(t *testing.T) {
	names := func() []string { return []string{"kvm-office", "kvm-office.fritz.box", "*.kvm.example.org"} }
	tests := []struct {
		host string
		want bool
	}{
		{"", true},
		{"192.168.1.50", true},
		{"192.168.1.50:80", true},
		{"[fd7a:115c:a1e0::1]", true},
		{"[fd7a:115c:a1e0::1]:443", true},
		{"[fe80::1%25eth0]", true},
		{"100.101.102.103", true},
		{"localhost:5173", true},
		{"kvm-office", true},
		{"jetkvm.local", true},
		{"JetKVM.Local.", true},
		{"kvm.home.arpa", true},
		{"kvm.lan", true},
		{"kvm.internal", true},
		{"kvm.tail1234.ts.net", true},
		{"kvm-office.fritz.box", true},
		{"KVM-Office.Fritz.Box:443", true},
		// A certificate's wildcard name covers one label.
		{"office.kvm.example.org", true},
		{"kvm.example.org", false},
		{"a.office.kvm.example.org", false},
		{".kvm.example.org", false},
		// Names public DNS answers for: what a rebinding page uses.
		{"rebind.attacker.example", false},
		{"kvm.example.com", false},
		{"other.fritz.box", false},
		{"192.168.1.50.nip.io", false},
		{"local.attacker.example", false},
		{"evilts.net", false},
		{"jetkvm", false}, // single label, not the device's name: a search domain may complete it
		{"[]", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := LocalHost(tt.host, names); got != tt.want {
				t.Fatalf("LocalHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestLocalHostLooksUpNamesLast(t *testing.T) {
	calls := 0
	names := func() []string { calls++; return []string{"jetkvm"} }
	for _, host := range []string{"192.168.1.50", "[fd7a:115c:a1e0::1]:443", "jetkvm.local", "kvm.lan"} {
		if !LocalHost(host, names) {
			t.Fatalf("LocalHost(%q) = false, want true", host)
		}
	}
	if calls != 0 {
		t.Fatalf("names looked up %d times for addresses and local names, want 0", calls)
	}
	if !LocalHost("jetkvm", names) || calls != 1 {
		t.Fatalf("the device's own name: looked up %d times, want 1", calls)
	}
	if LocalHost("jetkvm", nil) {
		t.Fatal("a single-label name with no names passed")
	}
}
