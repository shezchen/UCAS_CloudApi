package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/looplj/axonhub/internal/scopes"
)

// EmailVerificationChallenge stores a short-lived, one-time registration
// challenge. The verification code and request source are stored only as
// keyed digests so a database read cannot reveal either value.
type EmailVerificationChallenge struct {
	ent.Schema
}

func (EmailVerificationChallenge) Mixin() []ent.Mixin {
	return []ent.Mixin{TimeMixin{}}
}

func (EmailVerificationChallenge) Fields() []ent.Field {
	return []ent.Field{
		field.String("email").MaxLen(320),
		field.String("code_digest").MaxLen(64).Sensitive(),
		field.String("source_hash").MaxLen(64).Sensitive(),
		field.Time("expires_at"),
		field.Int("attempts").Default(0).Min(0),
		field.Time("consumed_at").Optional().Nillable(),
	}
}

func (EmailVerificationChallenge) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("email", "created_at"),
		index.Fields("source_hash", "created_at"),
		index.Fields("expires_at"),
		index.Fields("email", "consumed_at", "expires_at"),
	}
}

func (EmailVerificationChallenge) Annotations() []schema.Annotation {
	return []schema.Annotation{entgql.Skip()}
}

func (EmailVerificationChallenge) Policy() ent.Policy {
	return scopes.Policy{
		Query:    scopes.QueryPolicy{scopes.AlwaysDeny()},
		Mutation: scopes.MutationPolicy{scopes.AlwaysDeny()},
	}
}
