package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
)

// AccountUserQuotaPolicy configures allocation without owning or deleting usage history.
type AccountUserQuotaPolicy struct{ ent.Schema }

func (AccountUserQuotaPolicy) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "account_user_quota_policies"}}
}

func (AccountUserQuotaPolicy) Mixin() []ent.Mixin { return []ent.Mixin{mixins.TimeMixin{}} }

func (AccountUserQuotaPolicy) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("account_id").Positive(),
		field.String("upstream_account_id").Default("").MaxLen(200),
		field.Bool("enabled").Default(false),
		field.Time("enforced_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Int64("config_version").Default(1).Positive(),
		field.JSON("fallback_group_ids", []int64{}).Default([]int64{}),
		field.Int("carryover_seconds").Default(1800).Min(600).Max(86400),
		field.Int64("updated_by").Optional().Nillable(),
		field.String("history_note").Default("").MaxLen(1000),
		field.Time("history_verified_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (AccountUserQuotaPolicy) Indexes() []ent.Index {
	return []ent.Index{index.Fields("account_id").Unique()}
}
