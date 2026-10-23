//go:build integration

package repository

import (
	"context"
	"database/sql"
	"net/url"
	"testing"
	"testing/fstest"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const quotaMigrationName = "246_ablett_account_user_quota.sql"
const geminiImageMigrationName = "246_enable_gemini_image_generation_groups.sql"

func quotaUpgradeDatabase(t *testing.T) (*sql.DB, *dbent.Client) {
	t.Helper()
	ctx := context.Background()
	name := "quota_upgrade_" + uuid.New().String()[:8]
	_, err := integrationDB.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), "DROP DATABASE "+pq.QuoteIdentifier(name))
		require.NoError(t, err, "remove isolated quota upgrade database")
	})
	connection, err := url.Parse(integrationPostgresDSN)
	require.NoError(t, err)
	connection.Path = "/" + name
	connection.RawPath = ""
	db, err := sql.Open("postgres", connection.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var database, user string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT current_database(), current_user").Scan(&database, &user))
	require.Equal(t, name, database)
	require.Equal(t, "postgres", user)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	return db, client
}

func quotaUpgradeMigrations(t *testing.T, omitted string) fstest.MapFS {
	t.Helper()
	entries, err := migrations.FS.ReadDir(".")
	require.NoError(t, err)
	result := fstest.MapFS{}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == omitted {
			continue
		}
		data, err := migrations.FS.ReadFile(entry.Name())
		require.NoError(t, err)
		result[entry.Name()] = &fstest.MapFile{Data: data}
	}
	return result
}

func requireBothQuotaUpgradeMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, name := range []string{quotaMigrationName, geminiImageMigrationName} {
		var checksum string
		require.NoError(t, db.QueryRowContext(context.Background(), "SELECT checksum FROM schema_migrations WHERE filename=$1", name).Scan(&checksum))
		data, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		require.Equal(t, migrationChecksum(string(data)), checksum, "migration identity includes the complete filename")
	}
}

func TestAccountUserQuotaUpgradeFromMaintenance0217(t *testing.T) {
	db, client := quotaUpgradeDatabase(t)
	ctx := context.Background()
	require.NoError(t, applyMigrationsFS(ctx, db, quotaUpgradeMigrations(t, geminiImageMigrationName)))
	f := newQuotaFixtureForDatabase(t, db, client)
	before := f.ready(450, 300)
	ticket, err := f.admit(0, 8, f.now)
	require.NoError(t, err)
	require.NoError(t, f.repo.MarkSent(ctx, ticket.Token, f.now))
	require.NoError(t, f.repo.MarkUncertain(ctx, ticket.Token, f.now))
	gemini := f.createGroup(&service.Group{Name: "upgrade-gemini-" + uuid.NewString(), Platform: service.PlatformGemini})
	composite := f.createGroup(&service.Group{Name: "upgrade-composite-" + uuid.NewString(), Platform: service.PlatformComposite})
	require.NoError(t, client.Group.UpdateOneID(gemini.ID).SetAllowImageGeneration(false).Exec(ctx))
	require.NoError(t, client.Group.UpdateOneID(composite.ID).SetAllowImageGeneration(false).Exec(ctx))

	require.NoError(t, ApplyMigrations(ctx, db))
	requireBothQuotaUpgradeMigrations(t, db)
	f.repo = NewAccountUserQuotaRepository(client, db)
	after, err := f.repo.Get(ctx, f.account.ID, f.now)
	require.NoError(t, err)
	require.Equal(t, before.Cycle.ID, after.Cycle.ID)
	require.Equal(t, 450.0, after.Members[0].ActualCost)
	require.Equal(t, 8.0, after.Members[0].ReservedCost)
	require.Equal(t, 1, after.Uncertain)
	for _, group := range []struct {
		id      int64
		enabled bool
	}{{gemini.ID, true}, {composite.ID, false}} {
		var enabled bool
		require.NoError(t, db.QueryRowContext(ctx, "SELECT allow_image_generation FROM groups WHERE id=$1", group.id).Scan(&enabled))
		require.Equal(t, group.enabled, enabled)
	}
	_, err = f.billing.Apply(ctx, f.command(0, 4, 2, ticket))
	require.NoError(t, err)
	after, err = f.repo.Get(ctx, f.account.ID, f.now)
	require.NoError(t, err)
	require.Equal(t, 454.0, after.Members[0].ActualCost)
	require.Zero(t, after.Members[0].ReservedCost)
}

func TestAccountUserQuotaUpgradeFromOfficial0218(t *testing.T) {
	db, client := quotaUpgradeDatabase(t)
	ctx := context.Background()
	require.NoError(t, applyMigrationsFS(ctx, db, quotaUpgradeMigrations(t, quotaMigrationName)))
	f := newQuotaFixtureForDatabase(t, db, client)
	gemini := f.createGroup(&service.Group{Name: "adopt-gemini-" + uuid.NewString(), Platform: service.PlatformGemini})
	require.NoError(t, client.Group.UpdateOneID(gemini.ID).SetAllowImageGeneration(false).Exec(ctx))
	// Represent a completed official-version charge before Fork receipt columns exist.
	historical := f.command(0, 450, 300, nil)
	historical.Model = "gpt-5.1"
	historical.Normalize()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "UPDATE users SET balance=balance-450 WHERE id=$1", f.users[0].ID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_billing_dedup(request_id,api_key_id,request_fingerprint) VALUES($1,$2,$3)`, historical.RequestID, historical.APIKeyID, historical.RequestFingerprint)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,actual_cost,total_cost,rate_multiplier,created_at)
		VALUES($1,$2,$3,$4,'gpt-5.1',450,300,1,$5)`, f.users[0].ID, f.keys[0].ID, f.account.ID, historical.RequestID, f.now.Add(-10*time.Minute))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	require.NoError(t, ApplyMigrations(ctx, db))
	requireBothQuotaUpgradeMigrations(t, db)
	state := f.save(0, 0, 1, 2)
	state, err = f.repo.Initialize(ctx, f.account.ID, f.users[0].ID, state.Policy.Version, state.Cycle.ID, "已核对官方 v0.2.18 当前周期完整账单", f.now)
	require.NoError(t, err)
	require.Equal(t, 450.0, state.Members[0].ActualCost)
	require.Equal(t, 500.0, state.Members[0].Quota)
	require.Equal(t, 50.0, state.Members[0].Remaining)
	replay, err := f.billing.Apply(ctx, historical)
	require.NoError(t, err)
	require.False(t, replay.Applied, "the official-version fingerprint must still deduplicate the completed charge")
	state, err = f.repo.Initialize(ctx, f.account.ID, f.users[0].ID, state.Policy.Version, state.Cycle.ID, "再次核对旧版扣费重放与本期账单", f.now)
	require.NoError(t, err)
	require.Equal(t, 450.0, state.Members[0].ActualCost, "replay and reinitialization must not count the old charge twice")
	var balance float64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT balance FROM users WHERE id=$1", f.users[0].ID).Scan(&balance))
	require.Equal(t, 9550.0, balance, "initialization must not debit the historical charge again")
	var enabled bool
	require.NoError(t, db.QueryRowContext(ctx, "SELECT allow_image_generation FROM groups WHERE id=$1", gemini.ID).Scan(&enabled))
	require.False(t, enabled, "an already applied upstream migration must not reset a later admin choice")
	ticket, err := f.admit(0, 8, f.now)
	require.NoError(t, err)
	require.NoError(t, f.repo.MarkSent(ctx, ticket.Token, f.now))
	_, err = f.billing.Apply(ctx, f.command(0, 4, 2, ticket))
	require.NoError(t, err)
	state, err = f.repo.Get(ctx, f.account.ID, f.now)
	require.NoError(t, err)
	require.Equal(t, 454.0, state.Members[0].ActualCost)
	require.Zero(t, state.Members[0].ReservedCost)
}
