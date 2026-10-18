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

type AccountUserQuotaReservation struct{ ent.Schema }

func (AccountUserQuotaReservation) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "account_user_quota_reservations"}}
}

func (AccountUserQuotaReservation) Mixin() []ent.Mixin { return []ent.Mixin{mixins.TimeMixin{}} }

func (AccountUserQuotaReservation) Fields() []ent.Field {
	return []ent.Field{
		field.String("token").MaxLen(64).NotEmpty().Immutable(),
		field.Int64("account_id").Positive().Immutable(),
		field.Int64("cycle_id").Positive().Immutable(),
		field.Int64("user_id").Positive().Immutable(),
		field.Int64("api_key_id").Positive().Immutable(),
		field.Int64("original_group_id").Optional().Nillable().Immutable(),
		field.Int64("effective_group_id").Optional().Nillable().Immutable(),
		field.Int64("subscription_id").Optional().Nillable().Immutable(),
		field.String("request_id").MaxLen(128).Default("").Immutable(),
		field.Time("pricing_at").Immutable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Int64("config_version").Positive().Immutable(),
		field.Float("reserved_cost").Min(0).Immutable().SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Float("actual_cost").Default(0).Min(0).SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Enum("state").Values("reserved", "sent", "settled", "released", "uncertain").Default("reserved"),
		field.Time("lease_until").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("settled_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("billing_request_id").MaxLen(128).Optional().Nillable(),
		field.String("calibration_run_id").MaxLen(64).Optional().Nillable().Immutable(),
		field.Int("calibration_ordinal").Optional().Nillable().Min(1).Max(3).Immutable(),
		field.Int("calibration_max_requests").Optional().Nillable().Min(1).Max(3).Immutable(),
		field.Float("calibration_budget").Optional().Nillable().Positive().Immutable().SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.String("reconciliation_note").MaxLen(1000).Default(""),
	}
}

func (AccountUserQuotaReservation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("token").Unique(),
		index.Fields("calibration_run_id", "calibration_ordinal").Unique(),
		index.Fields("cycle_id", "user_id", "state"),
		index.Fields("account_id", "lease_until"),
	}
}
