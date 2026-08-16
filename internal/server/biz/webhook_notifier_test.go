package biz

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/llm/httpclient"
)

func newTestSystemServiceWithWebhookConfig(t *testing.T, client *ent.Client, cfg WebhookNotifierConfig) *SystemService {
	t.Helper()

	service := &SystemService{
		AbstractService: &AbstractService{
			db: client,
		},
		Cache: xcache.NewFromConfig[ent.System](xcache.Config{Mode: xcache.ModeMemory}),
	}

	ctx := ent.NewContext(context.Background(), client)
	ctx = authz.WithTestBypass(ctx)
	require.NoError(t, service.SetWebhookNotifierConfig(ctx, &cfg))

	return service
}

func TestWebhookNotifier_NotifyChannelAutoDisabled(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	var (
		receivedBody   string
		receivedHeader string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		receivedBody = string(body)
		receivedHeader = r.Header.Get("X-Axonhub-Event")

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	cfg := WebhookNotifierConfig{
		Targets: []WebhookTarget{
			{
				Name:      "default",
				Enabled:   true,
				URL:       server.URL,
				TimeoutMs: 1000,
				Headers: []objects.HeaderEntry{
					{Key: "X-AxonHub-Event", Value: "{{.Event}}"},
				},
				Body: `{"event":"{{.Event}}","channel":"{{.Channel.Name}}","status_code":{{.Trigger.StatusCode}},"threshold":{{.Trigger.Threshold}},"actual_count":{{.Trigger.ActualCount}}}`,
				// The httptest server listens on loopback, which the
				// public-network restriction rejects unless the target opts in.
				AllowPrivateNetwork: true,
			},
		},
		Subscriptions: []WebhookSubscription{
			{Event: EventChannelAutoDisabled, TargetNames: []string{"default"}},
		},
	}

	systemService := newTestSystemServiceWithWebhookConfig(t, client, cfg)
	notifier := NewWebhookNotifier(systemService, httpclient.NewHttpClient())
	require.True(t, notifier.restrictPublicNetwork, "delivery must run under the production restriction")

	notifier.NotifyChannelAutoDisabled(context.Background(), ChannelAutoDisabledEvent{
		ChannelID:       1,
		ChannelName:     "primary",
		ChannelProvider: "openai",
		ChannelBaseURL:  "https://api.openai.com",
		ChannelStatus:   "disabled",
		StatusCode:      429,
		Threshold:       3,
		ActualCount:     3,
		Reason:          "quota exhausted",
		OccurredAt:      time.Unix(1712812800, 0),
	})

	require.Equal(t, EventChannelAutoDisabled, receivedHeader)
	require.Contains(t, receivedBody, `"event":"channel.auto_disabled"`)
	require.Contains(t, receivedBody, `"channel":"primary"`)
	require.Contains(t, receivedBody, `"status_code":429`)
	require.Contains(t, receivedBody, `"threshold":3`)
	require.Contains(t, receivedBody, `"actual_count":3`)
}

// newTestSystemServiceWithStoredWebhookConfig writes the config straight to the
// system table, bypassing SetWebhookNotifierConfig validation. It reproduces a
// config stored before that validation existed, or one whose hostname started
// resolving to a private address after it was saved.
func newTestSystemServiceWithStoredWebhookConfig(t *testing.T, client *ent.Client, cfg WebhookNotifierConfig) *SystemService {
	t.Helper()

	service := &SystemService{
		AbstractService: &AbstractService{
			db: client,
		},
		Cache: xcache.NewFromConfig[ent.System](xcache.Config{Mode: xcache.ModeMemory}),
	}

	encoded, err := json.Marshal(&cfg)
	require.NoError(t, err)

	ctx := ent.NewContext(context.Background(), client)
	ctx = authz.WithTestBypass(ctx)
	require.NoError(t, service.setSystemValue(ctx, SystemKeyWebhookNotifierConfig, string(encoded)))

	return service
}

func TestWebhookNotifier_BlocksPrivateTargetWithoutOptIn(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	called := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := WebhookNotifierConfig{
		Targets: []WebhookTarget{
			{
				Name:      "loopback",
				Enabled:   true,
				URL:       server.URL,
				TimeoutMs: 1000,
				Body:      `{"event":"{{.Event}}"}`,
			},
		},
		Subscriptions: []WebhookSubscription{
			{Event: EventChannelAutoDisabled, TargetNames: []string{"loopback"}},
		},
	}

	systemService := newTestSystemServiceWithStoredWebhookConfig(t, client, cfg)
	notifier := NewWebhookNotifier(systemService, httpclient.NewHttpClient())
	require.True(t, notifier.restrictPublicNetwork)

	notifier.NotifyChannelAutoDisabled(context.Background(), ChannelAutoDisabledEvent{OccurredAt: time.Now()})

	require.False(t, called, "delivery to a loopback target must be blocked without AllowPrivateNetwork")
}

func TestSetWebhookNotifierConfig_RejectsPrivateTargetWithoutOptIn(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	service := &SystemService{
		AbstractService: &AbstractService{db: client},
		Cache:           xcache.NewFromConfig[ent.System](xcache.Config{Mode: xcache.ModeMemory}),
	}

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	tests := []struct {
		name      string
		target    WebhookTarget
		wantError string
	}{
		{
			name:      "loopback literal",
			target:    WebhookTarget{Name: "alertmanager", Enabled: true, URL: "http://127.0.0.1:9093/hook"},
			wantError: "restricted address",
		},
		{
			name:      "private literal",
			target:    WebhookTarget{Name: "alertmanager", Enabled: true, URL: "http://10.0.0.5/hook"},
			wantError: "restricted address",
		},
		{
			name:      "localhost name",
			target:    WebhookTarget{Name: "alertmanager", Enabled: true, URL: "http://localhost:9093/hook"},
			wantError: "not publicly routable",
		},
		{
			name:      "cloud metadata literal",
			target:    WebhookTarget{Name: "metadata", Enabled: true, URL: "http://169.254.169.254/latest/meta-data"},
			wantError: "restricted address",
		},
		{
			name:      "hostless file url",
			target:    WebhookTarget{Name: "file", Enabled: true, URL: "file:///etc/passwd"},
			wantError: "must be absolute and include a host",
		},
		{
			name:      "hostless file url with opt-in",
			target:    WebhookTarget{Name: "file", Enabled: true, URL: "file:///etc/passwd", AllowPrivateNetwork: true},
			wantError: "must be absolute and include a host",
		},
		{
			name:      "non-network scheme",
			target:    WebhookTarget{Name: "gopher", Enabled: true, URL: "gopher://example.com/hook"},
			wantError: "not supported",
		},
		{
			name:      "non-network scheme with opt-in",
			target:    WebhookTarget{Name: "gopher", Enabled: true, URL: "gopher://example.com/hook", AllowPrivateNetwork: true},
			wantError: "not supported",
		},
		{
			name:      "userinfo",
			target:    WebhookTarget{Name: "creds", Enabled: true, URL: "https://user:pass@example.com/hook"},
			wantError: "userinfo is not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := service.SetWebhookNotifierConfig(ctx, &WebhookNotifierConfig{
				Targets: []WebhookTarget{tt.target},
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantError)
			require.Contains(t, err.Error(), tt.target.Name)
		})
	}
}

func TestSetWebhookNotifierConfig_AcceptsAllowedTargets(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	service := &SystemService{
		AbstractService: &AbstractService{db: client},
		Cache:           xcache.NewFromConfig[ent.System](xcache.Config{Mode: xcache.ModeMemory}),
	}

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	cfg := WebhookNotifierConfig{
		Targets: []WebhookTarget{
			{Name: "public", Enabled: true, URL: "https://example.com/webhook"},
			{Name: "internal", Enabled: true, URL: "http://127.0.0.1:9093/hook", AllowPrivateNetwork: true},
			{Name: "draft", Enabled: false, URL: ""},
		},
	}

	require.NoError(t, service.SetWebhookNotifierConfig(ctx, &cfg))

	stored, err := service.WebhookNotifierConfig(ctx)
	require.NoError(t, err)
	require.Len(t, stored.Targets, 3)
	require.False(t, stored.Targets[0].AllowPrivateNetwork)
	require.True(t, stored.Targets[1].AllowPrivateNetwork)
}

func TestWebhookNotifier_SkipWhenTemplateInvalid(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	called := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	newConfig := func(body string) WebhookNotifierConfig {
		return WebhookNotifierConfig{
			Targets: []WebhookTarget{
				{
					Name:    "default",
					Enabled: true,
					URL:     server.URL,
					Body:    body,
					// Without the opt-in the loopback target would be dropped by
					// the public-network restriction, and the assertion below
					// would hold even if the invalid template were delivered.
					AllowPrivateNetwork: true,
				},
			},
			Subscriptions: []WebhookSubscription{
				{Event: EventChannelAutoDisabled, TargetNames: []string{"default"}},
			},
		}
	}

	// Positive control: the same target with a valid template must reach the
	// server, so the assertion below can only fail on the template.
	validService := newTestSystemServiceWithWebhookConfig(t, client, newConfig(`{"event":"{{.Event}}"}`))
	NewWebhookNotifier(validService, httpclient.NewHttpClient()).
		NotifyChannelAutoDisabled(context.Background(), ChannelAutoDisabledEvent{OccurredAt: time.Now()})
	require.True(t, called)

	called = false

	systemService := newTestSystemServiceWithWebhookConfig(t, client, newConfig(`{"event":"{{if .Event}}"}`))
	notifier := NewWebhookNotifier(systemService, httpclient.NewHttpClient())
	notifier.NotifyChannelAutoDisabled(context.Background(), ChannelAutoDisabledEvent{OccurredAt: time.Now()})

	require.False(t, called)
}

func TestNormalizeWebhookNotifierConfig_InitializesDefaults(t *testing.T) {
	cfg := WebhookNotifierConfig{}

	normalizeWebhookNotifierConfig(&cfg)

	require.NotNil(t, cfg.Targets)
	require.NotNil(t, cfg.Subscriptions)
}

func TestWebhookNotifier_SelectTargetsSkipsInvalidTargets(t *testing.T) {
	notifier := &WebhookNotifier{}
	targets := notifier.selectTargets(WebhookNotifierConfig{
		Targets: []WebhookTarget{
			{Name: "a", Enabled: true, URL: "https://example.com"},
			{Name: "b", Enabled: false, URL: "https://example.com"},
			{Name: "c", Enabled: true, URL: ""},
		},
		Subscriptions: []WebhookSubscription{
			{Event: EventChannelAutoDisabled, TargetNames: []string{"a", "b", "c", "missing"}},
		},
	}, EventChannelAutoDisabled)

	require.Len(t, targets, 1)
	require.Equal(t, "a", targets[0].Name)
}

func TestRenderWebhookTemplate_NoTemplate(t *testing.T) {
	result, err := renderWebhookTemplate("plain text", WebhookRenderContext{})
	require.NoError(t, err)
	require.Equal(t, "plain text", result)
}
