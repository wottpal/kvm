package kvm

import "github.com/pion/stun/v3"

// maxLocalSTUNServers bounds the STUN servers a local client may name.
const maxLocalSTUNServers = 4

// localSTUNServers keeps the STUN servers among a local client's ICE servers
// (D19): stun: URLs that pion parses, at most maxLocalSTUNServers, each at
// most 256 bytes. One pion rejected would fail the whole session; TURN, which
// relays and takes credentials, stays the cloud's; and stuns: (STUN over TLS)
// is not gathered by this pion.
func localSTUNServers(urls []string) []string {
	var out []string
	for _, u := range urls {
		if len(u) > 256 {
			continue
		}
		if uri, err := stun.ParseURI(u); err != nil || uri.Scheme != stun.SchemeTypeSTUN {
			continue
		}
		out = append(out, u)
		if len(out) == maxLocalSTUNServers {
			break
		}
	}
	return out
}
