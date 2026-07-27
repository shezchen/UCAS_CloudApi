package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/looplj/axonhub/internal/scopes"
)

// TokenWalletLedger is the permanent, append-only audit trail for donation
// wallet credits and consumption. Resource identifiers are intentionally plain
// immutable fields rather than required foreign keys: request/usage retention
// and channel expiry must never erase wallet history.
type TokenWalletLedger struct {
	ent.Schema
}

func (TokenWalletLedger) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
	}
}

func (TokenWalletLedger) Fields() []ent.Field {
	return []ent.Field{
		field.Int("user_id").
			Immutable().
			Positive(),
		field.Int("request_id").
			Immutable().
			Positive(),
		field.Int("usage_log_id").
			Optional().
			Immutable().
			Positive(),
		field.Int("channel_id").
			Optional().
			Immutable().
			Positive(),
		field.String("channel_name_snapshot").
			Default("").
			Immutable(),
		field.Enum("kind").
			Values("credit", "debit").
			Immutable(),
		field.Int64("amount_tokens").
			Immutable().
			Positive(),
		field.Int64("effective_tokens_snapshot").
			Default(0).
			Immutable().
			Min(0),
	}
}

func (TokenWalletLedger) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("request_id", "user_id", "kind").
			Unique().
			StorageKey("token_wallet_ledger_by_request_user_kind"),
		index.Fields("user_id", "created_at").
			StorageKey("token_wallet_ledger_by_user_created_at"),
		index.Fields("channel_id", "created_at").
			StorageKey("token_wallet_ledger_by_channel_created_at"),
	}
}

func (TokenWalletLedger) Policy() ent.Policy {
	return scopes.Policy{
		Query: scopes.QueryPolicy{
			scopes.OwnerRule(),
			scopes.UserOwnedQueryRule(),
		},
		Mutation: scopes.MutationPolicy{
			// Ledger rows are append-only and may only be created by trusted
			// internal settlement/restore paths under an explicit privacy
			// bypass. Even Owner cannot rewrite or delete accounting history.
			scopes.AlwaysDeny(),
		},
	}
}
