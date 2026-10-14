//go:build linux && arm

package kvm

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// withAuthMode runs the router with the given local auth mode (D15).
func withAuthMode(t *testing.T, mode string) *gin.Engine {
	t.Helper()
	saved := config
	config = &Config{LocalAuthMode: mode}
	t.Cleanup(func() { config = saved })
	return setupRouter()
}

func serve(r *gin.Engine, method, target, host string, headers map[string]string) int {
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestBrowserGuardCrossOrigin(t *testing.T) {
	r := withAuthMode(t, "noPassword")
	const host = "192.168.1.50"
	handshake := map[string]string{
		"Connection": "Upgrade", "Upgrade": "websocket",
		"Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==",
	}
	evilHandshake := map[string]string{"Origin": "https://evil.example"}
	for k, v := range handshake {
		evilHandshake[k] = v
	}

	// A page on another site opening signaling: the session it would get
	// types on the host.
	if code := serve(r, http.MethodGet, "/webrtc/signaling/client", host, evilHandshake); code != http.StatusForbidden {
		t.Fatalf("cross-origin signaling handshake: %d, want 403", code)
	}
	// A browser extension, even one without host permissions.
	extension := map[string]string{"Origin": "chrome-extension://abcdefghijklmnop"}
	for k, v := range handshake {
		extension[k] = v
	}
	if code := serve(r, http.MethodGet, "/webrtc/signaling/client", host, extension); code != http.StatusForbidden {
		t.Fatalf("extension signaling handshake: %d, want 403", code)
	}
	// A cross-site form post (JSON sent as text/plain needs no preflight).
	if code := serve(r, http.MethodPost, "/device/send-wol/not-a-mac", host,
		map[string]string{"Origin": "https://evil.example", "Content-Type": "text/plain"}); code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: %d, want 403", code)
	}
	// The device's own page and non-browser clients reach the handler.
	if code := serve(r, http.MethodPost, "/device/send-wol/not-a-mac", host,
		map[string]string{"Origin": "http://" + host}); code != http.StatusBadRequest {
		t.Fatalf("same-origin POST: %d, want the handler's 400", code)
	}
	if code := serve(r, http.MethodPost, "/device/send-wol/not-a-mac", host, nil); code != http.StatusBadRequest {
		t.Fatalf("POST without Origin: %d, want the handler's 400", code)
	}
	// Reading stays open across origins (the public status endpoint).
	if code := serve(r, http.MethodGet, "/device/status", host, map[string]string{"Origin": "https://app.jetkvm.com"}); code != http.StatusOK {
		t.Fatalf("cross-origin GET of /device/status: %d, want 200", code)
	}
}

func TestBrowserGuardDNSRebinding(t *testing.T) {
	for _, mode := range []string{"noPassword", ""} {
		r := withAuthMode(t, mode)
		if code := serve(r, http.MethodGet, "/device/status", "rebind.attacker.example", nil); code != http.StatusForbidden {
			t.Fatalf("mode %q, public host name: %d, want 403", mode, code)
		}
		for _, host := range []string{"192.168.1.50", "jetkvm.local", "localhost:8080", "kvm.tail1234.ts.net:80"} {
			if code := serve(r, http.MethodGet, "/device/status", host, nil); code != http.StatusOK {
				t.Fatalf("mode %q, host %s: %d, want 200", mode, host, code)
			}
		}
	}
	// With a password, the cookie and the login guard the device instead.
	r := withAuthMode(t, "password")
	if code := serve(r, http.MethodGet, "/device/status", "kvm.example.com", nil); code != http.StatusOK {
		t.Fatalf("password mode, public host name: %d, want 200", code)
	}
}

func TestAuthCookieIsSameSiteStrict(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth/login-local", nil)
	setAuthCookie(c, "token", authTokenMaxAge)
	cookie := w.Header().Get("Set-Cookie")
	for _, attr := range []string{"authToken=token", "HttpOnly", "SameSite=Strict"} {
		if !strings.Contains(cookie, attr) {
			t.Fatalf("Set-Cookie %q lacks %s", cookie, attr)
		}
	}
}

func TestBrowserAccessOff(t *testing.T) {
	saved := browserAccessOffFile
	browserAccessOffFile = filepath.Join(t.TempDir(), "browser-access.off")
	t.Cleanup(func() { browserAccessOffFile = saved })
	r := withAuthMode(t, "noPassword")
	const host = "192.168.1.50"
	browser := map[string]string{"User-Agent": "Mozilla/5.0 (Macintosh) AppleWebKit/605.1.15 Safari/605.1.15"}
	handshake := map[string]string{
		"Origin": "http://" + host, "Connection": "Upgrade", "Upgrade": "websocket",
		"Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==",
	}
	engine := map[string]string{"User-Agent": "Go-http-client/1.1"}

	if code := serve(r, http.MethodGet, "/device/status", host, browser); code != http.StatusOK {
		t.Fatalf("browser, access on: %d, want 200", code)
	}
	if err := rpcSetBrowserAccessState(false); err != nil {
		t.Fatal(err)
	}
	if rpcGetBrowserAccessState().Enabled {
		t.Fatal("access still reported on")
	}
	if code := serve(r, http.MethodGet, "/device/status", host, browser); code != http.StatusForbidden {
		t.Fatalf("browser, access off: %d, want 403", code)
	}
	if code := serve(r, http.MethodGet, "/webrtc/signaling/client", host, handshake); code != http.StatusForbidden {
		t.Fatalf("browser signaling, access off: %d, want 403", code)
	}
	if code := serve(r, http.MethodGet, "/device/status", host, engine); code != http.StatusOK {
		t.Fatalf("native client, access off: %d, want 200", code)
	}
	if err := rpcSetBrowserAccessState(true); err != nil {
		t.Fatal(err)
	}
	if code := serve(r, http.MethodGet, "/device/status", host, browser); code != http.StatusOK {
		t.Fatalf("browser, access back on: %d, want 200", code)
	}
}
