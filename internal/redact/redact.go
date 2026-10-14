// Package redact masks secrets in values that get logged (FORK.md D15). The
// app log ends up in the diagnostics download and in crash dumps, which users
// share; JSON-RPC parameters carry passwords (MQTT, Wi-Fi), the TLS private
// key and tokens.
package redact

import (
	"net/url"
	"strings"
)

// Mask replaces a secret's value.
const Mask = "[redacted]"

// secretStems are lower-case parts of key names whose values are secret.
var secretStems = []string{"password", "passphrase", "psk", "secret", "token", "privatekey", "private_key", "oidc"}

// IsSecretKey reports whether a key name denotes a secret.
func IsSecretKey(key string) bool {
	key = strings.ToLower(key)
	for _, stem := range secretStems {
		if strings.Contains(key, stem) {
			return true
		}
	}
	return false
}

// Params returns a copy of params, as decoded from JSON, with the value of
// every secret key masked at any depth, and the user info of URLs (a virtual
// media URL may carry credentials). params itself is not modified.
func Params(params map[string]any) map[string]any {
	if params == nil {
		return nil
	}
	return value(params).(map[string]any)
}

func value(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if IsSecretKey(k) {
				out[k] = Mask
			} else {
				out[k] = value(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = value(item)
		}
		return out
	case string:
		return urlWithoutUserInfo(v)
	default:
		return v
	}
}

// urlWithoutUserInfo masks the user info of s if it is a URL that has one.
func urlWithoutUserInfo(s string) string {
	if !strings.Contains(s, "://") || !strings.Contains(s, "@") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = url.User("redacted")
	return u.String()
}
