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

type AccountUserQuotaUsage struct{ ent.Schema }

func (AccountUserQuotaUsage) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "account_user_quota_usage"}}
}

func (AccountUserQuotaUsage) Mixin() []ent.Mixin { return []ent.Mixin{mixins.TimeMixin{}} }

func (AccountUserQuotaUsage) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("cycle_id").Positive(),
		field.Int64("user_id").Positive(),
		field.Float("actual_cost").Default(0).Min(0).SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Float("reserved_cost").Default(0).Min(0).SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
	}
}

func (AccountUserQuotaUsage) Indexes() []ent.Index {
	return []ent.Index{index.Fields("cycle_id", "user_id").Unique(), index.Fields("user_id")}
}
