package scopes

import (
	"context"

	"entgo.io/ent/entql"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/privacy"
)

// PersonalKeyProjectFilter is the interface for entities that support
// both project_id filtering and generic predicate injection.
type PersonalKeyProjectFilter interface {
	WhereProjectID(entql.IntP)
	Where(entql.P)
}

// UserPersonalAPIKeyReadRule allows the owner to view every API key and limits
// non-owner users to personal keys they created in the current project. It
// replaces UserProjectScopeReadRule for the APIKey schema.
func UserPersonalAPIKeyReadRule(requiredScope ScopeSlug) privacy.QueryRule {
	return privacy.FilterFunc(func(ctx context.Context, q privacy.Filter) error {
		currentUser, err := getUserFromContext(ctx)
		if err != nil {
			return privacy.Skipf("User not found in context")
		}

		// The system owner is the global administrator. Allow the query before
		// applying either the project boundary or the personal-key creator filter;
		// returning Allow after adding those predicates would make the later
		// OwnerRule unreachable and hide other users' personal keys from the owner.
		if currentUser.IsOwner {
			return privacy.Allowf("Owner user %d can query all API keys", currentUser.ID)
		}

		projectID, hasProjectID := contexts.GetProjectID(ctx)
		if !hasProjectID {
			return privacy.Skipf("Project ID not found in context")
		}

		// System-scope users can access any project, but personal keys are still filtered below
		if !HasSystemScope(currentUser, requiredScope) && !userHasProjectScope(currentUser, projectID, requiredScope) {
			return privacy.Skipf("User %d can not query project %d with scope %s", currentUser.ID, projectID, requiredScope)
		}

		pf, ok := q.(PersonalKeyProjectFilter)
		if !ok {
			return privacy.Skipf("Query does not support project_id and type filtering")
		}

		pf.WhereProjectID(entql.IntEQ(projectID))

		// Non-owner users must not see legacy user, service-account, or noauth
		// keys, even when those keys belong to the same project.
		pf.Where(entql.And(
			entql.FieldEQ("type", "personal"),
			entql.FieldEQ("user_id", currentUser.ID),
		))

		return privacy.Allowf("User %d can query project %d with scope %s", currentUser.ID, projectID, requiredScope)
	})
}
