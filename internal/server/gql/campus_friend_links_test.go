package gql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
)

func setupCampusFriendLinksResolver(t *testing.T) (*mutationResolver, *queryResolver, context.Context, *ent.Client) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	service := biz.NewSystemService(biz.SystemServiceParams{
		Ent:         client,
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
	})
	resolver := &Resolver{client: client, systemService: service}

	return &mutationResolver{resolver}, &queryResolver{resolver}, ent.NewContext(context.Background(), client), client
}

func campusFriendLinksUserContext(ctx context.Context, currentUser *ent.User, projectID ...int) context.Context {
	ctx = authz.NewUserContext(ctx, currentUser.ID)
	ctx = contexts.WithUser(ctx, currentUser)
	if len(projectID) != 0 {
		ctx = contexts.WithProjectID(ctx, projectID[0])
	}

	return ctx
}

func TestCampusFriendLinks_OwnerWritesAndCampusMembersRead(t *testing.T) {
	mutationResolver, queryResolver, baseCtx, client := setupCampusFriendLinksResolver(t)
	defer client.Close()

	setupCtx := authz.WithTestBypass(baseCtx)
	campus, err := client.Project.Create().
		SetName("campus").
		SetStatus(project.StatusActive).
		Save(setupCtx)
	require.NoError(t, err)

	owner, err := client.User.Create().
		SetEmail("owner@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		SetIsOwner(true).
		Save(setupCtx)
	require.NoError(t, err)

	member, err := client.User.Create().
		SetEmail("member@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		Save(setupCtx)
	require.NoError(t, err)
	_, err = client.UserProject.Create().
		SetUserID(member.ID).
		SetProjectID(campus.ID).
		Save(setupCtx)
	require.NoError(t, err)

	ownerCtx := campusFriendLinksUserContext(baseCtx, owner)
	memberCtx := campusFriendLinksUserContext(baseCtx, member, campus.ID)

	_, err = mutationResolver.UpdateCampusFriendLinks(memberCtx, []*CampusFriendLinkInput{{
		Name: "Member cannot write",
		URL:  "https://example.com",
	}})
	require.ErrorIs(t, err, ErrNotOwner)

	description := "  A maintained companion project  "
	ok, err := mutationResolver.UpdateCampusFriendLinks(ownerCtx, []*CampusFriendLinkInput{
		{
			Name:        "Campus project",
			URL:         "HTTPS://github.com/shezchen/UCAS_CloudApi",
			Description: &description,
		},
		{
			Name: "No description is valid",
			URL:  "https://github.com/positer/Newmark-Agent",
		},
	})
	require.NoError(t, err)
	require.True(t, ok)

	memberLinks, err := queryResolver.CampusFriendLinks(memberCtx)
	require.NoError(t, err)
	require.Equal(t, []*biz.CampusFriendLink{
		{
			Name:        "Campus project",
			URL:         "https://github.com/shezchen/UCAS_CloudApi",
			Description: "A maintained companion project",
		},
		{
			Name: "No description is valid",
			URL:  "https://github.com/positer/Newmark-Agent",
		},
	}, memberLinks)

	// The Owner can maintain and inspect the list without selecting a project.
	ownerLinks, err := queryResolver.CampusFriendLinks(ownerCtx)
	require.NoError(t, err)
	require.Equal(t, memberLinks, ownerLinks)
}

func TestCampusFriendLinks_RejectsNonMembersAndMissingProjectContext(t *testing.T) {
	mutationResolver, queryResolver, baseCtx, client := setupCampusFriendLinksResolver(t)
	defer client.Close()

	setupCtx := authz.WithTestBypass(baseCtx)
	campus, err := client.Project.Create().
		SetName("campus").
		SetStatus(project.StatusActive).
		Save(setupCtx)
	require.NoError(t, err)

	owner, err := client.User.Create().
		SetEmail("owner@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		SetIsOwner(true).
		Save(setupCtx)
	require.NoError(t, err)
	member, err := client.User.Create().
		SetEmail("member@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		Save(setupCtx)
	require.NoError(t, err)
	outsider, err := client.User.Create().
		SetEmail("outsider@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		Save(setupCtx)
	require.NoError(t, err)
	_, err = client.UserProject.Create().
		SetUserID(member.ID).
		SetProjectID(campus.ID).
		Save(setupCtx)
	require.NoError(t, err)

	ownerCtx := campusFriendLinksUserContext(baseCtx, owner)
	ok, err := mutationResolver.UpdateCampusFriendLinks(ownerCtx, []*CampusFriendLinkInput{{
		Name: "Configured link",
		URL:  "https://example.com",
	}})
	require.NoError(t, err)
	require.True(t, ok)

	memberWithoutProject := campusFriendLinksUserContext(baseCtx, member)
	_, err = queryResolver.CampusFriendLinks(memberWithoutProject)
	require.ErrorContains(t, err, "project ID not found")

	outsiderCtx := campusFriendLinksUserContext(baseCtx, outsider, campus.ID)
	_, err = queryResolver.CampusFriendLinks(outsiderCtx)
	require.ErrorContains(t, err, "not a project member")

	_, err = queryResolver.CampusFriendLinks(baseCtx)
	require.ErrorContains(t, err, "user not found")
}
