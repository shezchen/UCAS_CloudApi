package middleware

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		name       string
		clientIPs  []string
		blockedIPs []string
		want       bool
	}{
		{
			name:       "exact match",
			clientIPs:  []string{"203.0.113.10"},
			blockedIPs: []string{"203.0.113.10"},
			want:       true,
		},
		{
			name:       "cidr match",
			clientIPs:  []string{"203.0.113.10"},
			blockedIPs: []string{"203.0.113.0/24"},
			want:       true,
		},
		{
			name:       "trimmed blocked entry match",
			clientIPs:  []string{"203.0.113.10"},
			blockedIPs: []string{" 203.0.113.10 "},
			want:       true,
		},
		{
			name:       "ipv6 exact match",
			clientIPs:  []string{"2001:db8::10"},
			blockedIPs: []string{"2001:db8::10"},
			want:       true,
		},
		{
			name:       "ipv6 cidr match",
			clientIPs:  []string{"2001:db8::10"},
			blockedIPs: []string{"2001:db8::/64"},
			want:       true,
		},
		{
			name:       "later candidate match",
			clientIPs:  []string{"10.0.0.1", "203.0.113.10"},
			blockedIPs: []string{"203.0.113.10"},
			want:       true,
		},
		{
			name:       "invalid client candidate skipped",
			clientIPs:  []string{"bad-ip", "203.0.113.10"},
			blockedIPs: []string{"203.0.113.10"},
			want:       true,
		},
		{
			name:       "invalid blocked entries skipped",
			clientIPs:  []string{"203.0.113.10"},
			blockedIPs: []string{"bad-ip", "bad-prefix/33"},
			want:       false,
		},
		{
			name:       "no match",
			clientIPs:  []string{"203.0.113.10"},
			blockedIPs: []string{"198.51.100.0/24", "192.0.2.1"},
			want:       false,
		},
		{
			name:       "ipv4-mapped client does not evade exact entry",
			clientIPs:  []string{"::ffff:203.0.113.10"},
			blockedIPs: []string{"203.0.113.10"},
			want:       true,
		},
		{
			name:       "ipv4-mapped client does not evade cidr entry",
			clientIPs:  []string{"::ffff:203.0.113.10"},
			blockedIPs: []string{"203.0.113.0/24"},
			want:       true,
		},
		{
			name:       "ipv4-mapped blocked entry matches plain client",
			clientIPs:  []string{"203.0.113.10"},
			blockedIPs: []string{"::ffff:203.0.113.10"},
			want:       true,
		},
		{
			name:       "ipv4-mapped blocked prefix matches plain client",
			clientIPs:  []string{"203.0.113.10"},
			blockedIPs: []string{"::ffff:203.0.113.0/120"},
			want:       true,
		},
		{
			name:       "ipv4-mapped client still not blocked when outside range",
			clientIPs:  []string{"::ffff:198.51.100.10"},
			blockedIPs: []string{"203.0.113.0/24"},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBlockedIP(tt.clientIPs, tt.blockedIPs); got != tt.want {
				t.Fatalf("isBlockedIP() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClientIPCandidates(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, engine := gin.CreateTestContext(recorder)
	if err := engine.SetTrustedProxies(nil); err != nil {
		t.Fatalf("failed to set trusted proxies: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.10, 198.51.100.20")
	req.Header.Set("X-Real-IP", "192.0.2.30")
	ctx.Request = req

	got := clientIPCandidates(ctx)
	want := []string{"10.0.0.1", "203.0.113.10", "192.0.2.30"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("clientIPCandidates() = %#v, want %#v", got, want)
	}
}

// Behind a trusted proxy c.ClientIP() returns the forwarded value verbatim, so
// a caller can present its own address in IPv4-mapped IPv6 form. The blocklist
// must still match it.
func TestBlockedIPNotEvadedByMappedForwardedFor(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, engine := gin.CreateTestContext(recorder)
	if err := engine.SetTrustedProxies([]string{"10.0.0.1"}); err != nil {
		t.Fatalf("failed to set trusted proxies: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "::ffff:203.0.113.10")
	ctx.Request = req

	candidates := clientIPCandidates(ctx)
	if !isBlockedIP(candidates, []string{"203.0.113.10"}) {
		t.Fatalf("mapped address %#v evaded exact blocklist entry", candidates)
	}

	if !isBlockedIP(candidates, []string{"203.0.113.0/24"}) {
		t.Fatalf("mapped address %#v evaded CIDR blocklist entry", candidates)
	}
}

func TestClientIPCandidatesDeduplicates(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, engine := gin.CreateTestContext(recorder)
	if err := engine.SetTrustedProxies(nil); err != nil {
		t.Fatalf("failed to set trusted proxies: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.10, 198.51.100.20")
	req.Header.Set("X-Real-IP", "203.0.113.10")
	ctx.Request = req

	got := clientIPCandidates(ctx)
	want := []string{"203.0.113.10"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("clientIPCandidates() = %#v, want %#v", got, want)
	}
}
