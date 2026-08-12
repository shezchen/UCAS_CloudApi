package schema

import (
	"context"
	"fmt"

	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xapikey"
	"github.com/looplj/axonhub/internal/scopes"
)

type APIKey struct {
	ent.Schema
}

func (APIKey) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
		schematype.SoftDeleteMixin{},
	}
}

func (APIKey) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id").
			StorageKey("api_keys_by_user_id"),
		index.Fields("project_id").
			StorageKey("api_keys_by_project_id"),
		// The key column now stores only a redacted display value, so it is
		// no longer unique; uniqueness lives on key_hash. A plain index is
		// kept for the legacy plaintext fallback lookup during migration.
		index.Fields("key").
			StorageKey("api_keys_by_key"),
		index.Fields("key_hash").
			StorageKey("api_keys_by_key_hash").
			Unique(),
	}
}

func (APIKey) Fields() []ent.Field {
	return []ent.Field{
		field.Int("user_id").Optional().Immutable().
			Annotations(
				entgql.Skip(entgql.SkipMutationCreateInput, entgql.SkipMutationUpdateInput),
			).Comment("The creator of the API key"),
		field.Int("project_id").
			Immutable().
			Default(1).
			Comment("Project ID, default to 1 for backward compatibility").
			Annotations(
				entgql.Skip(entgql.SkipMutationUpdateInput),
			),
		field.String("key").
			Comment("Redacted display form of the key (prefix...suffix). The raw key is returned exactly once at creation/rotation time and is never persisted; verification uses key_hash.").
			Annotations(
				entgql.Skip(entgql.SkipMutationCreateInput, entgql.SkipMutationUpdateInput),
			),
		field.String("key_hash").
			Optional().
			Sensitive().
			Comment("SHA-256 hex digest of the raw key; the unique lookup handle for authentication. Optional only to tolerate legacy rows before the startup backfill runs.").
			Annotations(
				entgql.Skip(entgql.SkipAll),
			),
		field.String("key_prefix").
			Optional().
			Default("").
			Comment("Leading characters of the raw key for display/identification.").
			Annotations(
				entgql.Skip(entgql.SkipMutationCreateInput, entgql.SkipMutationUpdateInput),
			),
		field.String("name"),
		field.Enum("type").
			Values("user", "service_account", "noauth", "personal").
			Default("user").
			Comment("API Key type: user, service_account, noauth, or personal").Annotations(
			entgql.Skip(entgql.SkipMutationUpdateInput),
		),
		field.Enum("status").Values("enabled", "disabled", "archived").Default("enabled").Annotations(
			entgql.Skip(entgql.SkipMutationCreateInput, entgql.SkipMutationUpdateInput),
		),
		field.Strings("scopes").
			Comment("API Key specific scopes. For user type: default read_channels, write_requests (immutable). For service_account: custom scopes.").
			Default([]string{"read_channels", "write_requests"}).
			Optional(),
		field.JSON("profiles", &objects.APIKeyProfiles{}).
			Default(&objects.APIKeyProfiles{}).
			Optional().
			Annotations(
				entgql.Skip(entgql.SkipMutationCreateInput, entgql.SkipMutationUpdateInput),
			),
	}
}

func (APIKey) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Unique().
			Immutable().
			Annotations(
				entgql.Skip(entgql.SkipMutationCreateInput, entgql.SkipMutationUpdateInput),
				entgql.Directives(forceResolver()),
			).
			Ref("api_keys").Field("user_id"),
		edge.From("project", Project.Type).
			Unique().
			Immutable().
			Required().
			Annotations(
				entgql.Skip(entgql.SkipMutationUpdateInput),
			).
			Ref("api_keys").Field("project_id"),
		edge.To("requests", Request.Type).
			Annotations(
				entgql.Skip(entgql.SkipMutationCreateInput, entgql.SkipMutationUpdateInput),
				entgql.RelayConnection(),
			),
	}
}

func (APIKey) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entgql.Mutations(entgql.MutationCreate(), entgql.MutationUpdate()),
	}
}

// Hooks make key hashing transparent for every write path (biz services,
// backup restore, tests): whenever a raw key is written, only its SHA-256
// hash and a redacted display form are persisted.
//
// The hook is the sole authority on key material:
//
//   - A raw key always determines its own hash and prefix. A caller cannot
//     pair a raw key with a hash of something else, which would leave the
//     value shown to the user unable to authenticate while the caller's
//     string could.
//   - A redacted display value carries nothing to derive from, so it may only
//     be written together with the hash it belongs to. That covers the
//     backfill, the legacy read-repair and backup restore, and rejects a
//     write that would leave key and key_hash describing different secrets.
//
// The hook intentionally uses the generic mutation API (string field names)
// instead of the typed *gen.APIKeyMutation so that entc can load this schema
// even before the key_hash/key_prefix accessors have been generated.
func (APIKey) Hooks() []ent.Hook {
	return []ent.Hook{
		hook.On(
			func(next ent.Mutator) ent.Mutator {
				return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
					value, ok := m.Field("key")

					raw, isString := value.(string)
					if !ok || !isString || raw == "" {
						return next.Mutate(ctx, m)
					}

					if xapikey.IsRedacted(raw) {
						if hash, _ := m.Field("key_hash"); hash == nil || hash == "" {
							return nil, fmt.Errorf(
								"api key: a redacted key must be written together with its key_hash")
						}

						return next.Mutate(ctx, m)
					}

					if err := m.SetField("key_hash", xapikey.Hash(raw)); err != nil {
						return nil, err
					}

					if err := m.SetField("key_prefix", xapikey.Prefix(raw)); err != nil {
						return nil, err
					}

					if err := m.SetField("key", xapikey.Redact(raw)); err != nil {
						return nil, err
					}

					return next.Mutate(ctx, m)
				})
			},
			ent.OpCreate|ent.OpUpdate|ent.OpUpdateOne,
		),
	}
}

// Policy 定义 APIKey 的权限策略.
func (APIKey) Policy() ent.Policy {
	return scopes.Policy{
		Query: scopes.QueryPolicy{
			scopes.UserPersonalAPIKeyReadRule(scopes.ScopeReadAPIKeys),  // User 主体：project_id 过滤 + personal key 仅创建者可见
			scopes.APIKeyProjectScopeReadRule(scopes.ScopeReadAPIKeys),  // API key 主体：用于 OpenAPI 走 service account 读 APIKey
			scopes.OwnerRule(), // owner 用户可以访问所有 API Keys
		},
		Mutation: scopes.MutationPolicy{
			scopes.UserProjectScopeWriteRule(scopes.ScopeWriteAPIKeys),   // 需要 API Keys 写入权限
			scopes.APIKeyProjectScopeWriteRule(scopes.ScopeWriteAPIKeys), // API key scope + project 校验
			scopes.OwnerRule(), // owner 用户可以修改所有 API Keys
		},
	}
}
