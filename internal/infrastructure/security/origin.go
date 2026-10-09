package security

import (
	"net"
	"net/url"
	"strings"
)

func SameOrigin(origin, host string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, host)
}

func ClientIP(peer, forwardedFor string, trustProxy bool, trustedProxies []string) string {
	if !trustProxy || !isTrustedProxy(peer, trustedProxies) {
		return peer
	}
	current := peer
	parts := strings.Split(forwardedFor, ",")
	for index := len(parts) - 1; index >= 0; index-- {
		if !isTrustedProxy(current, trustedProxies) {
			break
		}
		candidate := strings.TrimSpace(parts[index])
		if net.ParseIP(candidate) == nil {
			break
		}
		current = candidate
	}
	return current
}

func SecureCookie(setting, requestScheme, peer, forwardedProto string, trustProxy bool, trustedProxies []string) bool {
	value := strings.ToLower(setting)
	if value == "true" {
		return true
	}
	if value != "auto" {
		return false
	}
	return strings.EqualFold(requestScheme, "https") || (isTrustedProxy(peer, trustedProxies) && strings.EqualFold(strings.TrimSpace(strings.Split(forwardedProto, ",")[len(strings.Split(forwardedProto, ","))-1]), "https") && trustProxy)
}

func isTrustedProxy(value string, trustedProxies []string) bool {
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil {
		return false
	}
	for _, raw := range trustedProxies {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "/") {
			item += "/128"
			if ip.To4() != nil {
				item = strings.TrimSuffix(item, "/128") + "/32"
			}
		}
		_, network, err := net.ParseCIDR(item)
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}
