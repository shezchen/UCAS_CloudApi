package api

import (
	"context"
	"fmt"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
)

var logger *log.Logger

func initLogger(l *log.Logger) {
	logger = l.WithName("api")
}

func oauthHTTPClientForCaller(
	ctx context.Context,
	base *httpclient.HttpClient,
	proxy *httpclient.ProxyConfig,
) (*httpclient.HttpClient, error) {
	if currentUser, ok := contexts.GetUser(ctx); ok && currentUser != nil && !currentUser.IsOwner {
		if err := biz.ValidateDonationProxy(ctx, proxy); err != nil {
			return nil, fmt.Errorf("OAuth proxy is not allowed: %w", err)
		}

		if proxy != nil && proxy.Type == httpclient.ProxyTypeURL {
			return base.WithProxy(proxy).WithPublicNetworkOnly(), nil
		}

		return base.WithPublicNetworkOnly(), nil
	}

	if proxy != nil && proxy.Type == httpclient.ProxyTypeURL && proxy.URL != "" {
		return base.WithProxy(proxy), nil
	}

	return base, nil
}
