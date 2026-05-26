package vpnc

import (
	"fmt"
	"net"
	"strings"

	"github.com/WarrDoge/sslcon/utils"
)

func NormalizeDNSDomains(domains []string) []string {
	seen := make(map[string]struct{}, len(domains))
	normalized := make([]string, 0, len(domains))

	for _, domain := range domains {
		d := strings.ToLower(strings.TrimSpace(domain))
		d = strings.TrimPrefix(d, ".")
		if d == "" {
			continue
		}
		if _, exists := seen[d]; exists {
			continue
		}
		seen[d] = struct{}{}
		normalized = append(normalized, d)
	}

	return normalized
}

func routeToCIDR(route string) (string, error) {
	route = strings.TrimSpace(route)
	if route == "" {
		return "", fmt.Errorf("route entry is empty")
	}

	if _, _, err := net.ParseCIDR(route); err == nil {
		return route, nil
	}

	parts := strings.Split(route, "/")
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid route %q", route)
	}
	if net.ParseIP(parts[1]) == nil {
		return "", fmt.Errorf("invalid route %q", route)
	}

	cidr := utils.IpMaskToCIDR(route)
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		return "", fmt.Errorf("invalid route %q", route)
	}
	return cidr, nil
}
