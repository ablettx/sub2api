package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
)

type AccountUserQuotaMember struct{ ent.Schema }

func (AccountUserQuotaMember) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "account_user_quota_members"}}
}

func (AccountUserQuotaMember) Mixin() []ent.Mixin { return []ent.Mixin{mixins.TimeMixin{}} }

func (AccountUserQuotaMember) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("account_id").Positive(),
		field.Int64("user_id").Positive(),
		field.Bool("active").Default(true),
	}
}

func (AccountUserQuotaMember) Indexes() []ent.Index {
	return []ent.Index{index.Fields("account_id", "user_id").Unique(), index.Fields("user_id")}
}
