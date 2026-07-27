package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/looplj/axonhub/internal/scopes"
)

// UserTokenWallet stores the materialized balance of a user's permanent
// donation reward wallet. TokenWalletLedger remains the auditable source of
// every balance change.
type UserTokenWallet struct {
	ent.Schema
}

func (UserTokenWallet) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
	}
}

func (UserTokenWallet) Fields() []ent.Field {
	return []ent.Field{
		field.Int("user_id").
			Immutable().
			Positive(),
		field.Int64("balance_tokens").
			Default(0).
			Min(0),
		field.Int64("lifetime_credited_tokens").
			Default(0).
			Min(0),
		field.Int64("lifetime_debited_tokens").
			Default(0).
			Min(0),
		field.Int64("version").
			Default(0).
			Min(0),
	}
}

func (UserTokenWallet) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id").
			Unique().
			StorageKey("user_token_wallets_by_user_id"),
	}
}

func (UserTokenWallet) Policy() ent.Policy {
	return scopes.Policy{
		Query: scopes.QueryPolicy{
			scopes.OwnerRule(),
			scopes.UserOwnedQueryRule(),
		},
		Mutation: scopes.MutationPolicy{
			scopes.OwnerRule(),
		},
	}
}
