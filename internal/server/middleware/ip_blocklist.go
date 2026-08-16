package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
)

// WithIPBlocklist blocks external API requests from configured IP addresses or CIDR ranges.
func WithIPBlocklist(systemService *biz.SystemService) gin.HandlerFunc {
	return func(c *gin.Context) {
		clientIPs := clientIPCandidates(c)
		if len(clientIPs) == 0 {
			c.Next()
			return
		}

		ctx := authz.WithSystemBypass(c.Request.Context(), "ip-blocklist-middleware")
		settings := systemService.SecuritySettingsOrDefault(ctx)
		if !isBlockedIP(clientIPs, settings.BlockedIPs) {
			c.Next()
			return
		}

		AbortWithError(c, http.StatusForbidden, errors.New("IP address is blocked"))
	}
}

// clientIPCandidates collects the addresses evaluated against the blocklist.
//
// c.ClientIP() already honors the engine's trusted-proxy configuration
// (server.go calls SetTrustedProxies): behind a configured proxy it returns the
// proxy-supplied client address, otherwise the direct peer. The raw
// X-Forwarded-For / X-Real-IP values are additionally included as
// defense-in-depth candidates.
//
// SECURITY NOTE: X-Forwarded-For and X-Real-IP are client-controlled when the
// deployment is not behind a trusted proxy, so they must never be used for
// allow decisions. They only widen the set of addresses compared against the
// blocklist, so a spoofed candidate can make a caller match a blocked entry
// (blocking itself) but cannot remove a match produced by another candidate.
// Evasion is prevented by normalizing every address in isBlockedAddr rather
// than by this list. Deployments terminating TLS behind a proxy should
// configure server.trusted_proxies so c.ClientIP() resolves the real client
// address.
func clientIPCandidates(c *gin.Context) []string {
	candidates := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}

		if _, ok := seen[value]; ok {
			return
		}

		seen[value] = struct{}{}
		candidates = append(candidates, value)
	}

	add(c.ClientIP())

	if xff := c.Request.Header.Get("X-Forwarded-For"); xff != "" {
		before, _, _ := strings.Cut(xff, ",")
		add(before)
	}

	add(c.Request.Header.Get("X-Real-IP"))

	return candidates
}

func isBlockedIP(clientIPs []string, blockedIPs []string) bool {
	for _, clientIP := range clientIPs {
		clientAddr, err := netip.ParseAddr(clientIP)
		if err != nil {
			log.Warn(context.Background(), "failed to parse client IP", log.String("client_ip", clientIP), log.Cause(err))
			continue
		}

		if isBlockedAddr(clientAddr, blockedIPs) {
			return true
		}
	}

	return false
}

// isBlockedAddr compares clientAddr against the blocklist.
//
// Every address is unmapped first: netip treats an IPv4 address and its
// IPv4-mapped IPv6 form as distinct, and Prefix.Contains never matches an
// IPv4-mapped address against an IPv4 prefix. Without normalization a caller
// behind a trusted proxy could send X-Forwarded-For: ::ffff:<blocked-ip> and
// evade both the exact and the CIDR entries.
func isBlockedAddr(clientAddr netip.Addr, blockedIPs []string) bool {
	clientAddr = clientAddr.Unmap()

	for _, item := range blockedIPs {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		if strings.Contains(item, "/") {
			prefix, err := netip.ParsePrefix(item)
			if err != nil {
				log.Warn(context.Background(), "failed to parse blocked IP prefix", log.String("blocked_ip", item), log.Cause(err))
				continue
			}

			if unmapPrefix(prefix).Contains(clientAddr) {
				return true
			}

			continue
		}

		blockedAddr, err := netip.ParseAddr(item)
		if err != nil {
			log.Warn(context.Background(), "failed to parse blocked IP", log.String("blocked_ip", item), log.Cause(err))
			continue
		}

		if blockedAddr.Unmap() == clientAddr {
			return true
		}
	}

	return false
}

// unmapPrefix rewrites an IPv4-mapped IPv6 prefix (e.g. ::ffff:203.0.113.0/120)
// to its IPv4 equivalent so it can match unmapped client addresses. Prefixes
// that are not IPv4-mapped, or whose length cannot describe an IPv4 network,
// are returned unchanged.
func unmapPrefix(prefix netip.Prefix) netip.Prefix {
	addr := prefix.Addr()
	if !addr.Is4In6() || prefix.Bits() < 96 {
		return prefix
	}

	return netip.PrefixFrom(addr.Unmap(), prefix.Bits()-96)
}
