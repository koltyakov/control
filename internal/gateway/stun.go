package gateway

import (
	"errors"

	"github.com/pion/stun/v4"
)

func validateSTUNServers(servers []string) error {
	if len(servers) > 8 {
		return errors.New("at most eight gateway STUN URLs are allowed")
	}
	for _, server := range servers {
		u, err := stun.ParseURI(server)
		if len(server) > 256 || err != nil || (u.Scheme != stun.SchemeTypeSTUN && u.Scheme != stun.SchemeTypeSTUNS) {
			return errors.New("gateway STUN URLs must be valid stun: or stuns: URLs")
		}
	}
	return nil
}
