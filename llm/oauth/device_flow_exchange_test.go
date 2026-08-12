package oauth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
)

// recordingTokenExchanger records every access token it is asked to exchange.
type recordingTokenExchanger struct {
	mu           sync.Mutex
	accessTokens []string
	expiresAt    int64
}

func (e *recordingTokenExchanger) record(accessToken string) (string, int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.accessTokens = append(e.accessTokens, accessToken)

	return "exchanged-for-" + accessToken, e.expiresAt, nil
}

func (e *recordingTokenExchanger) calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]string(nil), e.accessTokens...)
}

func (e *recordingTokenExchanger) Exchange(_ context.Context, accessToken string) (string, int64, error) {
	return e.record(accessToken)
}

func (e *recordingTokenExchanger) ExchangeWithClient(
	_ context.Context,
	_ *httpclient.HttpClient,
	accessToken string,
) (string, int64, error) {
	return e.record(accessToken)
}

// The exchanger owns caching, so every GetToken must reach it. Caching here
// would override the exchanger's own expiry buffer with a shorter one.
func TestDeviceFlowProvider_GetToken_DelegatesEveryExchangeToTheExchanger(t *testing.T) {
	t.Parallel()

	exchanger := &recordingTokenExchanger{expiresAt: time.Now().Add(time.Hour).Unix()}
	provider := NewDeviceFlowProvider(DeviceFlowProviderParams{
		HTTPClient:     httpclient.NewHttpClient(),
		TokenExchanger: exchanger,
		Credentials: &OAuthCredentials{
			AccessToken:  "oauth-access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(time.Hour),
		},
	})

	ctx := context.Background()

	for range 3 {
		token, err := provider.GetToken(ctx)
		require.NoError(t, err)
		assert.Equal(t, "exchanged-for-oauth-access-token", token)
	}

	require.Len(t, exchanger.calls(), 3)
}

// blockingTokenExchanger lets a test hold one exchange open while another
// caller arrives.
type blockingTokenExchanger struct {
	fn func(accessToken string) (string, int64, error)
}

func (e *blockingTokenExchanger) Exchange(_ context.Context, accessToken string) (string, int64, error) {
	return e.fn(accessToken)
}

func (e *blockingTokenExchanger) ExchangeWithClient(
	_ context.Context,
	_ *httpclient.HttpClient,
	accessToken string,
) (string, int64, error) {
	return e.fn(accessToken)
}

// A caller must never be handed a token exchanged for a credential that is no
// longer current, which is what a single shared in-flight exchange would do.
func TestDeviceFlowProvider_GetToken_UpdatedCredentialsAreNotServedAStaleExchange(t *testing.T) {
	t.Parallel()

	var (
		started = make(chan struct{})
		release = make(chan struct{})
	)

	exchanger := &blockingTokenExchanger{
		fn: func(accessToken string) (string, int64, error) {
			if accessToken == "old-access-token" {
				close(started)
				<-release
			}

			return "exchanged-for-" + accessToken, time.Now().Add(time.Hour).Unix(), nil
		},
	}

	provider := NewDeviceFlowProvider(DeviceFlowProviderParams{
		HTTPClient:     httpclient.NewHttpClient(),
		TokenExchanger: exchanger,
		Credentials: &OAuthCredentials{
			AccessToken:  "old-access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(time.Hour),
		},
	})

	firstToken := make(chan string, 1)

	go func() {
		token, err := provider.GetToken(context.Background())
		assert.NoError(t, err)
		firstToken <- token
	}()

	<-started

	provider.UpdateCredentials(&OAuthCredentials{
		AccessToken:  "new-access-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	})

	secondToken := make(chan string, 1)

	go func() {
		token, err := provider.GetToken(context.Background())
		assert.NoError(t, err)
		secondToken <- token
	}()

	select {
	case token := <-secondToken:
		assert.Equal(t, "exchanged-for-new-access-token", token)
	case <-time.After(5 * time.Second):
		t.Error("the second caller must not wait on an exchange for another credential")
	}

	close(release)

	select {
	case token := <-firstToken:
		assert.Equal(t, "exchanged-for-old-access-token", token)
	case <-time.After(5 * time.Second):
		t.Error("the first caller never completed")
	}
}
