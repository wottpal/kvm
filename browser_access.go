package kvm

import (
	"fmt"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/jetkvm/kvm/internal/webguard"
)

// browserAccessOffFile turns browser access off while it exists (FORK.md
// D16): the HTTP and HTTPS servers then refuse web browsers, so only native
// clients such as BlitzKVM reach the device. A flag file rather than a
// configuration key, like developer mode's: stock firmware ignores it, so
// going back to it turns browser access on again, and a factory reset
// removes it.
var browserAccessOffFile = "/userdata/jetkvm/browser-access.off"

type BrowserAccessState struct {
	Enabled bool `json:"enabled"`
}

// browserAccessEnabled reports whether web browsers may use the device. It
// reads the flag every time, so removing the file over SSH takes effect at
// once.
func browserAccessEnabled() bool {
	_, err := os.Stat(browserAccessOffFile)
	return err != nil
}

func rpcGetBrowserAccessState() BrowserAccessState {
	return BrowserAccessState{Enabled: browserAccessEnabled()}
}

// rpcSetBrowserAccessState turns browser access on or off. Sessions already
// running stay; the cloud's sessions do not go through the device's web
// servers and are not affected.
func rpcSetBrowserAccessState(enabled bool) error {
	if enabled {
		if err := os.Remove(browserAccessOffFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to turn browser access on: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(browserAccessOffFile, nil, 0644); err != nil {
		return fmt.Errorf("failed to turn browser access off: %w", err)
	}
	return nil
}

// browserAccessMiddleware refuses web browsers while browser access is off
// (D16). Native clients pass; the flag is read only for browsers.
func browserAccessMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if webguard.IsBrowser(c.Request) && !browserAccessEnabled() {
			sendErrorJsonThenAbort(c, http.StatusForbidden,
				"Browser access is off on this JetKVM. Connect with BlitzKVM, which can turn it back on.")
			return
		}
		c.Next()
	}
}
