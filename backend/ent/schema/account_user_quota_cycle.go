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

type AccountUserQuotaCycle struct{ ent.Schema }

func (AccountUserQuotaCycle) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "account_user_quota_cycles"}}
}

func (AccountUserQuotaCycle) Mixin() []ent.Mixin { return []ent.Mixin{mixins.TimeMixin{}} }

func (AccountUserQuotaCycle) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("account_id").Positive(),
		field.Time("starts_at").Immutable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("resets_at").Immutable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Bool("ready").Default(false),
		field.Int64("version").Default(1).Positive(),
		field.Float("account_stats_cost").Default(0).Min(0).SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Float("estimate").Optional().Nillable().Positive().SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Time("estimate_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Int64("estimate_source_cycle_id").Optional().Nillable(),
		field.Time("observed_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Float("used_percent").Optional().Nillable(),
		field.Time("missing_since").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Int64("backfill_through_id").Default(0).NonNegative(),
	}
}

func (AccountUserQuotaCycle) Indexes() []ent.Index {
	return []ent.Index{index.Fields("account_id", "resets_at").Unique(), index.Fields("account_id", "starts_at")}
}
