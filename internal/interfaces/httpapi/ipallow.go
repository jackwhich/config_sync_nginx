package httpapi

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"nginx_updata_config/internal/config"
)

func effectiveClientIP(r *http.Request, access config.IPAccessParsed) net.IP {
	host, err := hostOnlyFromRemoteAddr(r.RemoteAddr)
	if err != nil {
		return nil
	}
	remoteIP := net.ParseIP(host)
	if remoteIP == nil {
		return nil
	}

	if len(access.Trusted) > 0 && ipInAnyNet(remoteIP, access.Trusted) {
		fwd := strings.TrimSpace(strings.Join(r.Header.Values("X-Forwarded-For"), ","))
		if fwd == "" {
			if len(r.Header.Values("X-Real-IP")) != 1 {
				return nil
			}
			fwd = strings.TrimSpace(r.Header.Get("X-Real-IP"))
		}
		if fwd == "" {
			return nil
		}
		// Walk from the nearest proxy towards the client. Anything to the left
		// of the first untrusted hop is attacker-controlled and is ignored.
		hops := strings.Split(fwd, ",")
		ip := remoteIP
		for i := len(hops) - 1; i >= 0 && ipInAnyNet(ip, access.Trusted); i-- {
			ip = net.ParseIP(strings.TrimSpace(hops[i]))
			if ip == nil {
				return nil
			}
		}
		return ip
	}

	return remoteIP
}

func hostOnlyFromRemoteAddr(remoteAddr string) (string, error) {
	if remoteAddr == "" {
		return "", errors.New("empty RemoteAddr")
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		if net.ParseIP(remoteAddr) != nil {
			return remoteAddr, nil
		}
		return "", err
	}
	return host, nil
}

func ipInAnyNet(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func clientIPAllowed(ip net.IP, access config.IPAccessParsed) bool {
	if !access.Enabled {
		return true
	}
	if ip == nil {
		return false
	}
	return ipInAnyNet(ip, access.Rules)
}
