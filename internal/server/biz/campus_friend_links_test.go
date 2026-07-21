package biz

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

func setupCampusFriendLinksTestService(t *testing.T) (*SystemService, context.Context, *ent.Client) {
	t.Helper()

	service, client := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	return service, ctx, client
}

func TestSystemService_CampusFriendLinks_DefaultsToEmptyWithoutSeeding(t *testing.T) {
	service, ctx, client := setupCampusFriendLinksTestService(t)
	defer client.Close()

	links, err := service.CampusFriendLinks(ctx)
	require.NoError(t, err)
	require.NotNil(t, links)
	require.Empty(t, links)

	count, err := client.System.Query().Where(system.KeyEQ(SystemKeyCampusFriendLinks)).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, count, "reading the default must not seed hard-coded or empty links")
}

func TestSystemService_CampusFriendLinks_NormalizesAndPreservesOrder(t *testing.T) {
	service, ctx, client := setupCampusFriendLinksTestService(t)
	defer client.Close()

	input := []CampusFriendLink{
		{
			Name:        "  Campus project  ",
			URL:         " HTTPS://GitHub.COM/shezchen/UCAS_CloudApi ",
			Description: "  Campus self-hosted API project  ",
		},
		{
			Name: "Newmark Agent",
			URL:  "https://github.com/positer/Newmark-Agent",
		},
		{
			Name: "Intranet resource",
			URL:  "HTTP://Example.COM/docs",
		},
	}
	require.NoError(t, service.SetCampusFriendLinks(ctx, input))

	links, err := service.CampusFriendLinks(ctx)
	require.NoError(t, err)
	require.Equal(t, []CampusFriendLink{
		{
			Name:        "Campus project",
			URL:         "https://github.com/shezchen/UCAS_CloudApi",
			Description: "Campus self-hosted API project",
		},
		{Name: "Newmark Agent", URL: "https://github.com/positer/Newmark-Agent"},
		{Name: "Intranet resource", URL: "http://example.com/docs"},
	}, links)
}

func TestSystemService_SetCampusFriendLinks_RejectsUnsafeOrAmbiguousEntries(t *testing.T) {
	tooManyLinks := make([]CampusFriendLink, maxCampusFriendLinks+1)
	for i := range tooManyLinks {
		tooManyLinks[i] = CampusFriendLink{
			Name: fmt.Sprintf("Link %d", i),
			URL:  fmt.Sprintf("https://example.com/%d", i),
		}
	}

	tests := []struct {
		name        string
		links       []CampusFriendLink
		errContains string
	}{
		{
			name:        "empty name",
			links:       []CampusFriendLink{{Name: "  ", URL: "https://example.com"}},
			errContains: "empty name",
		},
		{
			name:        "empty URL",
			links:       []CampusFriendLink{{Name: "Example", URL: "  "}},
			errContains: "empty URL",
		},
		{
			name:        "relative URL",
			links:       []CampusFriendLink{{Name: "Example", URL: "/docs"}},
			errContains: "absolute HTTP(S) URL",
		},
		{
			name:        "unsafe scheme",
			links:       []CampusFriendLink{{Name: "Example", URL: "javascript:alert(1)"}},
			errContains: "absolute HTTP(S) URL",
		},
		{
			name:        "embedded credentials",
			links:       []CampusFriendLink{{Name: "Example", URL: "https://user:password@example.com"}},
			errContains: "must not include user credentials",
		},
		{
			name: "duplicate names are case insensitive",
			links: []CampusFriendLink{
				{Name: "Project", URL: "https://example.com/one"},
				{Name: "project", URL: "https://example.com/two"},
			},
			errContains: "duplicates a link name",
		},
		{
			name: "duplicate URLs use normalized scheme and host",
			links: []CampusFriendLink{
				{Name: "One", URL: "https://EXAMPLE.com/path"},
				{Name: "Two", URL: "HTTPS://example.COM/path"},
			},
			errContains: "duplicates a link URL",
		},
		{
			name:        "too many links",
			links:       tooManyLinks,
			errContains: "at most",
		},
		{
			name: "description is limited",
			links: []CampusFriendLink{{
				Name:        "Example",
				URL:         "https://example.com",
				Description: strings.Repeat("a", maxCampusFriendLinkDescriptionRunes+1),
			}},
			errContains: "description exceeds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, ctx, client := setupCampusFriendLinksTestService(t)
			defer client.Close()

			err := service.SetCampusFriendLinks(ctx, tt.links)
			require.ErrorContains(t, err, tt.errContains)

			count, countErr := client.System.Query().Where(system.KeyEQ(SystemKeyCampusFriendLinks)).Count(ctx)
			require.NoError(t, countErr)
			require.Zero(t, count, "invalid input must not persist a partial configuration")
		})
	}
}

func TestSystemService_CampusFriendLinks_RejectsInvalidStoredConfiguration(t *testing.T) {
	service, ctx, client := setupCampusFriendLinksTestService(t)
	defer client.Close()

	_, err := client.System.Create().
		SetKey(SystemKeyCampusFriendLinks).
		SetValue(`[{"name":"Unsafe","url":"javascript:alert(1)"}]`).
		Save(ctx)
	require.NoError(t, err)

	_, err = service.CampusFriendLinks(ctx)
	require.ErrorContains(t, err, "invalid stored campus friend links")
}
