package server

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAllowPrivateNetworkCORS(t *testing.T) {
	tests := []struct {
		name                string
		allowPrivateNetwork bool
		allowNoAuth         bool
		want                bool
	}{
		{name: "off by default", want: false},
		{name: "enabled with authentication", allowPrivateNetwork: true, want: true},
		{
			name:                "withheld when the API needs no key",
			allowPrivateNetwork: true,
			allowNoAuth:         true,
			want:                false,
		},
		{name: "no auth alone does not enable it", allowNoAuth: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := Config{}
			config.CORS.AllowPrivateNetwork = tt.allowPrivateNetwork
			config.API.Auth.AllowNoAuth = tt.allowNoAuth

			require.Equal(t, tt.want, config.AllowPrivateNetworkCORS())
		})
	}
}
