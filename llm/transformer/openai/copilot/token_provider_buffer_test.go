package copilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
)

// The exchanger treats a Copilot token as expired tokenExpiryBuffer early so a
// long request cannot outlive it. Nothing above it may serve the token past
// that point with a shorter skew of its own.
func TestCopilotTokenProvider_GetToken_HonorsExchangerExpiryBuffer(t *testing.T) {
	var exchanges atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := exchanges.Add(1)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(copilotTokenResponse{
			Token: "copilot_token_" + strconv.FormatInt(count, 10),
			// Inside the exchanger's 5 minute buffer but outside a 1 minute one.
			ExpiresAt: time.Now().Add(90 * time.Second).Unix(),
		})
	}))
	defer srv.Close()

	hc := httpclient.NewHttpClientWithClient(srv.Client())
	exchanger := NewTokenExchanger(TokenExchangerParams{
		HTTPClient: hc,
		Endpoint:   srv.URL + "/copilot_internal/v2/token",
	})

	provider := oauth.NewDeviceFlowProvider(oauth.DeviceFlowProviderParams{
		HTTPClient:     hc,
		TokenExchanger: exchanger,
		Credentials: &oauth.OAuthCredentials{
			AccessToken:  "oauth-access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(time.Hour),
		},
	})

	ctx := context.Background()

	first, err := provider.GetToken(ctx)
	require.NoError(t, err)
	require.Equal(t, "copilot_token_1", first)

	second, err := provider.GetToken(ctx)
	require.NoError(t, err)
	require.Equal(t, "copilot_token_2", second,
		"a token inside the exchanger's expiry buffer must be re-exchanged, not reused")
	require.Equal(t, int64(2), exchanges.Load())
}

// A token comfortably outside the buffer is still served from the exchanger's
// own cache, so removing the extra layer does not add an exchange per request.
func TestCopilotTokenProvider_GetToken_ReusesTokenOutsideExpiryBuffer(t *testing.T) {
	var exchanges atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := exchanges.Add(1)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(copilotTokenResponse{
			Token:     "copilot_token_" + strconv.FormatInt(count, 10),
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
	}))
	defer srv.Close()

	hc := httpclient.NewHttpClientWithClient(srv.Client())
	exchanger := NewTokenExchanger(TokenExchangerParams{
		HTTPClient: hc,
		Endpoint:   srv.URL + "/copilot_internal/v2/token",
	})

	provider := oauth.NewDeviceFlowProvider(oauth.DeviceFlowProviderParams{
		HTTPClient:     hc,
		TokenExchanger: exchanger,
		Credentials: &oauth.OAuthCredentials{
			AccessToken:  "oauth-access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(time.Hour),
		},
	})

	ctx := context.Background()

	for range 3 {
		token, err := provider.GetToken(ctx)
		require.NoError(t, err)
		require.Equal(t, "copilot_token_1", token)
	}

	require.Equal(t, int64(1), exchanges.Load())
}
