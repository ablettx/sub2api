//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func (f *quotaFixture) createAccount(account *service.Account) *service.Account {
	f.t.Helper()
	account = mustCreateAccount(f.t, testEntClient(f.t), account)
	f.accountIDs = append(f.accountIDs, account.ID)
	return account
}

func (f *quotaFixture) createGroup(group *service.Group) *service.Group {
	f.t.Helper()
	group = mustCreateGroup(f.t, testEntClient(f.t), group)
	f.groupIDs = append(f.groupIDs, group.ID)
	return group
}

// Production ledger rows deliberately prevent hard deletion of their owners.
// Tests use committed billing transactions, so roll back their owned data here
// in dependency order without weakening those constraints or touching other tests.
func (f *quotaFixture) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := integrationDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	userIDs := make([]int64, 0, len(f.users))
	for _, user := range f.users {
		userIDs = append(userIDs, user.ID)
	}
	var keyHashes pq.StringArray
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(array_agg(encode(sha256(convert_to(key, 'UTF8')), 'hex')), '{}'::text[])
		FROM api_keys WHERE user_id = ANY($1)`, pq.Array(userIDs)).Scan(&keyHashes); err != nil {
		return fmt.Errorf("quota fixture key hashes: %w", err)
	}
	for _, step := range []struct {
		query string
		ids   []int64
	}{
		{`DELETE FROM usage_logs WHERE account_id = ANY($1)`, f.accountIDs},
		{`DELETE FROM usage_billing_dedup WHERE account_id = ANY($1)`, f.accountIDs},
		{`DELETE FROM usage_billing_dedup_archive WHERE account_id = ANY($1)`, f.accountIDs},
		{`DELETE FROM account_user_quota_reservations WHERE account_id = ANY($1)`, f.accountIDs},
		{`DELETE FROM account_user_quota_usage WHERE cycle_id IN (SELECT id FROM account_user_quota_cycles WHERE account_id = ANY($1))`, f.accountIDs},
		{`DELETE FROM account_user_quota_members WHERE account_id = ANY($1)`, f.accountIDs},
		{`DELETE FROM account_user_quota_cycles WHERE account_id = ANY($1)`, f.accountIDs},
		{`DELETE FROM account_user_quota_policies WHERE account_id = ANY($1)`, f.accountIDs},
		{`DELETE FROM usage_dashboard_hourly_users WHERE user_id = ANY($1)`, userIDs},
		{`DELETE FROM usage_dashboard_daily_users WHERE user_id = ANY($1)`, userIDs},
		{`DELETE FROM user_subscriptions WHERE user_id = ANY($1)`, userIDs},
		{`DELETE FROM api_keys WHERE user_id = ANY($1)`, userIDs},
		{`DELETE FROM users WHERE id = ANY($1)`, userIDs},
		{`DELETE FROM account_groups WHERE account_id = ANY($1)`, f.accountIDs},
		{`DELETE FROM scheduler_outbox WHERE account_id = ANY($1)`, f.accountIDs},
		{`DELETE FROM scheduler_outbox WHERE group_id = ANY($1)`, f.groupIDs},
		{`DELETE FROM usage_group_daily_rollups WHERE group_id = ANY($1)`, f.groupIDs},
		{`DELETE FROM accounts WHERE id = ANY($1)`, f.accountIDs},
		{`DELETE FROM groups WHERE id = ANY($1)`, f.groupIDs},
	} {
		if len(step.ids) == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, step.query, pq.Array(step.ids)); err != nil {
			return fmt.Errorf("quota fixture cleanup %s: %w", step.query, err)
		}
	}
	// Key deletion itself enqueues invalidations; remove them after all deletes.
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = ANY($1)`, keyHashes); err != nil {
		return fmt.Errorf("quota fixture auth invalidations: %w", err)
	}
	return tx.Commit()
}

func TestAccountUserQuotaFixtureCleanupPreservesOtherFixtures(t *testing.T) {
	other := newQuotaFixture(t)
	other.ready(40, 300)
	otherGroup := other.createGroup(&service.Group{Name: "quota-retained-" + uuid.NewString()})
	var removed *quotaFixture
	t.Run("committed_billing_and_extra_resources", func(t *testing.T) {
		f := newQuotaFixture(t)
		removed = f
		f.ready(450, 300)
		client := testEntClient(t)
		group := f.createGroup(&service.Group{Name: "quota-cleanup-" + uuid.NewString()})
		account := f.createAccount(&service.Account{Name: "quota-extra-" + uuid.NewString()})
		mustBindAccountToGroup(t, client, account.ID, group.ID, 0)
		mustCreateApiKey(t, client, &service.APIKey{UserID: f.users[0].ID, Key: "sk-cleanup-" + uuid.NewString()})
		mustCreateSubscription(t, client, &service.UserSubscription{UserID: f.users[0].ID, GroupID: group.ID})
		ticket, err := f.admit(0, 8, f.now)
		require.NoError(t, err)
		require.NoError(t, f.repo.MarkSent(f.ctx, ticket.Token, f.now))
		require.NoError(t, f.repo.MarkUncertain(f.ctx, ticket.Token, f.now))
		_, err = integrationDB.ExecContext(f.ctx, `INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,actual_cost,total_cost,rate_multiplier,created_at)
			VALUES($1,$2,$3,$4,'gpt-5.1',450,300,1,$5)`, f.users[0].ID, f.keys[0].ID, f.account.ID, uuid.NewString(), f.now)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(f.ctx, `UPDATE usage_billing_dedup SET created_at=$2 WHERE account_id=$1`, f.account.ID, f.now.Add(-48*time.Hour))
		require.NoError(t, err)
		require.NoError(t, NewDashboardAggregationRepository(integrationDB).CleanupUsageBillingDedup(f.ctx, f.now.Add(-24*time.Hour)))
	})
	require.NotNil(t, removed)
	for _, accountID := range removed.accountIDs {
		for _, table := range []string{"accounts", "usage_logs", "usage_billing_dedup", "usage_billing_dedup_archive", "account_user_quota_policies", "account_user_quota_cycles", "account_user_quota_members", "account_user_quota_reservations"} {
			column := "account_id"
			if table == "accounts" {
				column = "id"
			}
			var count int
			require.NoError(t, integrationDB.QueryRowContext(other.ctx, "SELECT count(*) FROM "+table+" WHERE "+column+"=$1", accountID).Scan(&count))
			require.Zero(t, count, "fixture left %s rows for account %d", table, accountID)
		}
	}
	for _, user := range removed.users {
		var count int
		require.NoError(t, integrationDB.QueryRowContext(other.ctx, `SELECT count(*) FROM users WHERE id=$1`, user.ID).Scan(&count))
		require.Zero(t, count, "fixture must release its users for subsequent suites")
	}
	var count int
	require.NoError(t, integrationDB.QueryRowContext(other.ctx, `SELECT count(*) FROM groups WHERE id=ANY($1)`, pq.Array(removed.groupIDs)).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, integrationDB.QueryRowContext(other.ctx, `SELECT count(*) FROM groups WHERE id=$1`, otherGroup.ID).Scan(&count))
	require.Equal(t, 1, count, "cleanup must retain another fixture's group")
	state, err := other.repo.Get(other.ctx, other.account.ID, other.now)
	require.NoError(t, err)
	require.Equal(t, 40.0, state.Members[0].ActualCost, "cleanup must retain another fixture's committed ledger")
}
