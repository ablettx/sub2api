//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type quotaFixture struct {
	t          *testing.T
	ctx        context.Context
	now        time.Time
	reset      time.Time
	account    *service.Account
	users      []*service.User
	keys       []*service.APIKey
	repo       service.AccountUserQuotaRepository
	billing    service.UsageBillingRepository
	accountIDs []int64
	groupIDs   []int64
}

func TestAccountUserQuotaRepositoryArchivedReceiptsAndMirroredLogs(t *testing.T) {
	f := newQuotaFixture(t)
	cmd := f.command(0, 450, 300, nil)
	_, err := f.billing.Apply(f.ctx, cmd)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(f.ctx, `UPDATE usage_billing_dedup SET created_at=$2 WHERE request_id=$1`, cmd.RequestID, f.now.Add(-48*time.Hour))
	require.NoError(t, err)
	require.NoError(t, NewDashboardAggregationRepository(integrationDB).CleanupUsageBillingDedup(f.ctx, f.now.Add(-24*time.Hour)))
	_, err = integrationDB.ExecContext(f.ctx, `INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,actual_cost,total_cost,rate_multiplier,created_at)
		VALUES($1,$2,$3,$4,'gpt-5.1',450,300,1,$5)`, f.users[0].ID, f.keys[0].ID, f.account.ID, cmd.RequestID, f.now)
	require.NoError(t, err)
	s := f.save(0, 0, 1, 2)
	s, err = f.repo.Initialize(f.ctx, f.account.ID, f.users[0].ID, s.Policy.Version, s.Cycle.ID, "已核对归档收据和当前周期历史账单", f.now)
	require.NoError(t, err)
	require.Equal(t, 450.0, s.Members[0].ActualCost)
	require.Equal(t, 300.0, s.Cycle.AccountStatsCost)
	result, err := f.billing.Apply(f.ctx, cmd)
	require.NoError(t, err)
	require.False(t, result.Applied)
	s, err = f.repo.Get(f.ctx, f.account.ID, f.now)
	require.NoError(t, err)
	require.Equal(t, 450.0, s.Members[0].ActualCost)
}

func TestAccountUserQuotaRepositoryUnverifiableHistoryAndCalibrationBudget(t *testing.T) {
	f := newQuotaFixture(t)
	cmd := f.command(0, 450, 300, nil)
	cmd.AccountUserExecutionStartedAt = time.Time{}
	_, err := f.billing.Apply(f.ctx, cmd)
	require.NoError(t, err)
	s := f.save(0, 0, 1, 2)
	_, err = f.repo.Initialize(f.ctx, f.account.ID, f.users[0].ID, s.Policy.Version, s.Cycle.ID, "核对发现存在缺少执行时间的历史账单", f.now)
	require.Error(t, err)
	_, err = integrationDB.ExecContext(f.ctx, `UPDATE usage_billing_dedup SET execution_started_at=$2 WHERE request_id=$1`, cmd.RequestID, f.now.Add(-time.Minute))
	require.NoError(t, err)
	s, err = f.repo.Initialize(f.ctx, f.account.ID, f.users[0].ID, s.Policy.Version, s.Cycle.ID, "已核对并补齐历史账单的真实执行时间", f.now)
	require.NoError(t, err)
	calibration := &service.AccountUserQuotaCalibration{RunID: uuid.NewString(), Ordinal: 1, MaxRequests: 2, Budget: 0.01, CycleID: s.Cycle.ID}
	in := &service.AccountUserQuotaAdmission{AccountID: f.account.ID, UserID: f.users[0].ID, APIKeyID: f.keys[0].ID, ReservedCost: 0.008, PricingAt: f.now, Calibration: calibration}
	ticket, err := f.repo.Admit(f.ctx, in, f.now)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	require.NoError(t, f.repo.MarkSent(f.ctx, ticket.Token, f.now))
	_, err = f.billing.Apply(f.ctx, f.command(0, 0.009, 0.006, ticket))
	require.NoError(t, err)
	calibration.Ordinal = 2
	_, err = f.repo.Admit(f.ctx, in, f.now)
	require.Error(t, err, "actual sample charge limits remaining calibration budget")
}

func TestAccountUserQuotaRepositoryMigrationsAndRestartContinuity(t *testing.T) {
	f := newQuotaFixture(t)
	before := f.ready(450, 300)
	ticket, err := f.admit(0, 8, f.now)
	require.NoError(t, err)
	require.NoError(t, f.repo.MarkSent(f.ctx, ticket.Token, f.now))
	var checksum string
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT checksum FROM schema_migrations WHERE filename='246_ablett_account_user_quota.sql'`).Scan(&checksum))
	require.NoError(t, ApplyMigrations(f.ctx, integrationDB))
	fsys := fstest.MapFS{}
	entries, err := migrations.FS.ReadDir(".")
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := migrations.FS.ReadFile(entry.Name())
		require.NoError(t, err)
		fsys[entry.Name()] = &fstest.MapFile{Data: data}
	}
	// An upstream addition with the same numeric prefix has its own identity.
	fsys["246_upstream_quota_upgrade_fixture.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE IF NOT EXISTS ablett_upgrade_test_marker(id BIGINT PRIMARY KEY);`)}
	require.NoError(t, applyMigrationsFS(f.ctx, integrationDB, fsys))
	require.NoError(t, applyMigrationsFS(f.ctx, integrationDB, fsys))
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), `DELETE FROM schema_migrations WHERE filename='246_upstream_quota_upgrade_fixture.sql'`)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(context.Background(), `DROP TABLE IF EXISTS ablett_upgrade_test_marker`)
		require.NoError(t, err)
	})
	restarted := NewAccountUserQuotaRepository(testEntClient(t), integrationDB)
	after, err := restarted.Get(f.ctx, f.account.ID, f.now)
	require.NoError(t, err)
	require.Equal(t, before.Cycle.ID, after.Cycle.ID)
	require.Equal(t, 450.0, after.Members[0].ActualCost)
	require.Equal(t, 8.0, after.Members[0].ReservedCost)
	var afterChecksum string
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT checksum FROM schema_migrations WHERE filename='246_ablett_account_user_quota.sql'`).Scan(&afterChecksum))
	require.Equal(t, checksum, afterChecksum)
	_, err = NewUsageBillingRepository(testEntClient(t), integrationDB).Apply(f.ctx, f.command(0, 4, 2, ticket))
	require.NoError(t, err)
	after, err = restarted.Get(f.ctx, f.account.ID, f.now)
	require.NoError(t, err)
	require.Equal(t, 454.0, after.Members[0].ActualCost)
	require.Zero(t, after.Members[0].ReservedCost)
}

func TestAccountUserQuotaRepositoryMultipleKeysAccountsAndDisabledMembers(t *testing.T) {
	f := newQuotaFixture(t)
	state := f.ready(200, 300)
	secondKey := mustCreateApiKey(t, testEntClient(t), &service.APIKey{UserID: f.users[0].ID, Key: "sk-second-" + uuid.NewString()})
	ticket, err := f.repo.Admit(f.ctx, &service.AccountUserQuotaAdmission{AccountID: f.account.ID, UserID: f.users[0].ID, APIKeyID: secondKey.ID, ReservedCost: 150, PricingAt: f.now}, f.now)
	require.NoError(t, err)
	require.NoError(t, f.repo.MarkSent(f.ctx, ticket.Token, f.now))
	cmd := f.command(0, 150, 75, ticket)
	cmd.APIKeyID = secondKey.ID
	_, err = f.billing.Apply(f.ctx, cmd)
	require.NoError(t, err)
	state, err = f.repo.Get(f.ctx, f.account.ID, f.now)
	require.NoError(t, err)
	require.Equal(t, 350.0, state.Members[0].ActualCost)
	other := f.createAccount(&service.Account{Name: "other-quota-" + uuid.NewString(), Platform: service.PlatformOpenAI, Credentials: map[string]any{"chatgpt_account_id": uuid.NewString()}, Extra: f.account.Extra})
	cmd = f.command(0, 40, 30, nil)
	cmd.AccountID = other.ID
	_, err = f.billing.Apply(f.ctx, cmd)
	require.NoError(t, err)
	otherState, err := f.repo.Save(f.ctx, other.ID, f.users[0].ID, &service.SaveAccountUserQuotaInput{Enabled: true, MemberUserIDs: []int64{f.users[0].ID}, CarryoverSeconds: 1800}, f.now)
	require.NoError(t, err)
	otherState, err = f.repo.Initialize(f.ctx, other.ID, f.users[0].ID, otherState.Policy.Version, otherState.Cycle.ID, "已核对另一个账号当前周期的历史收据", f.now)
	require.NoError(t, err)
	require.Equal(t, 40.0, otherState.Members[0].ActualCost)
	_, err = integrationDB.ExecContext(f.ctx, `UPDATE users SET status='disabled' WHERE id=$1`, f.users[0].ID)
	require.NoError(t, err)
	state = f.save(state.Policy.Version, 0, 1, 2)
	require.Len(t, state.Members, 3)
	require.Equal(t, 350.0, state.Members[0].ActualCost)
	_, err = f.admit(0, 1, f.now)
	require.Error(t, err, "disabled user cannot execute, and their share is not reassigned")
	state, err = f.repo.Save(f.ctx, f.account.ID, f.users[1].ID, &service.SaveAccountUserQuotaInput{Enabled: false, ExpectedVersion: state.Policy.Version,
		MemberUserIDs: state.Policy.MemberUserIDs, CarryoverSeconds: 1800}, f.now)
	require.NoError(t, err, "disabled member must not prevent disabling the policy")
	require.Equal(t, 350.0, state.Members[0].ActualCost)
}

func TestAccountUserQuotaRepositoryNewMemberKeepsPriorCycleCharges(t *testing.T) {
	f := newQuotaFixture(t)
	_, err := f.billing.Apply(f.ctx, f.command(3, 100, 0, nil))
	require.NoError(t, err)
	state := f.ready(450, 300)
	cycleID := state.Cycle.ID
	state = f.save(state.Policy.Version, 0, 1, 2, 3)
	require.Equal(t, cycleID, state.Cycle.ID)
	require.Equal(t, 375.0, state.Members[3].Quota)
	require.Equal(t, 100.0, state.Members[3].ActualCost, "joining must include charges before membership")
	state, err = f.repo.Initialize(f.ctx, f.account.ID, f.users[0].ID, state.Policy.Version, cycleID, "已复核新成员加入前的完整周期收据", f.now)
	require.NoError(t, err)
	require.Equal(t, 100.0, state.Members[3].ActualCost, "reinitialization must not repeat the historical charge")
}

func TestAccountUserQuotaRepositorySentRequestSettlesAfterRealtimeDecrease(t *testing.T) {
	f := newQuotaFixture(t)
	before := f.ready(490, 300)
	ticket, err := f.admit(0, 8, f.now)
	require.NoError(t, err)
	require.NoError(t, f.repo.MarkSent(f.ctx, ticket.Token, f.now))
	state := f.observe(25, f.now.Add(time.Second), f.reset)
	require.Equal(t, 400.0, state.Members[0].Quota)
	require.Equal(t, 490.0, state.Members[0].ActualCost)
	require.Equal(t, 8.0, state.Members[0].ReservedCost)
	_, err = f.admit(0, 1, f.now.Add(time.Second))
	require.Error(t, err, "the lower limit must block the next execution")
	cmd := f.command(0, 12, 2, ticket)
	result, err := f.billing.Apply(f.ctx, cmd)
	require.NoError(t, err, "a previously sent request still settles its full actual cost")
	require.True(t, result.Applied)
	result, err = f.billing.Apply(f.ctx, cmd)
	require.NoError(t, err)
	require.False(t, result.Applied)
	state, err = f.repo.Get(f.ctx, f.account.ID, f.now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, before.Cycle.ID, state.Cycle.ID)
	require.Equal(t, 502.0, state.Members[0].ActualCost)
	require.Zero(t, state.Members[0].ReservedCost)
	require.Zero(t, state.Members[0].Available)
	var balance float64
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT balance FROM users WHERE id=$1`, f.users[0].ID).Scan(&balance))
	require.Equal(t, 9498.0, balance, "quota accounting must not debit twice or truncate the tail")
}

func TestAccountUserQuotaRepositoryDisabledPolicyStillTracksActualCharges(t *testing.T) {
	f := newQuotaFixture(t)
	state := f.ready(450, 300)
	cycleID := state.Cycle.ID
	state, err := f.repo.Save(f.ctx, f.account.ID, f.users[0].ID, &service.SaveAccountUserQuotaInput{
		Enabled: false, ExpectedVersion: state.Policy.Version, MemberUserIDs: state.Policy.MemberUserIDs, CarryoverSeconds: 1800,
	}, f.now)
	require.NoError(t, err)
	cmd := f.command(0, 20, 10, nil)
	_, err = f.billing.Apply(f.ctx, cmd)
	require.NoError(t, err)
	state = f.save(state.Policy.Version, 0, 1, 2)
	require.Equal(t, cycleID, state.Cycle.ID)
	require.Equal(t, 470.0, state.Members[0].ActualCost, "re-enabling cannot grant a second budget")
	restarted := NewAccountUserQuotaRepository(testEntClient(t), integrationDB)
	state, err = restarted.Initialize(f.ctx, f.account.ID, f.users[0].ID, state.Policy.Version, cycleID, "已核对停用期间和重新启用后的全部扣费", f.now)
	require.NoError(t, err)
	require.Equal(t, 470.0, state.Members[0].ActualCost)
	require.Equal(t, 310.0, state.Cycle.AccountStatsCost)
}

func newQuotaFixture(t *testing.T) *quotaFixture {
	t.Helper()
	f := &quotaFixture{t: t, ctx: context.Background(), now: time.Now().UTC().Truncate(time.Second)}
	t.Cleanup(func() { require.NoError(t, f.cleanup(), "clean quota fixture") })
	f.reset = f.now.Add(6 * 24 * time.Hour)
	client := testEntClient(t)
	f.repo = NewAccountUserQuotaRepository(client, integrationDB)
	f.billing = NewUsageBillingRepository(client, integrationDB)
	f.account = f.createAccount(&service.Account{Name: "quota-" + uuid.NewString(), Platform: service.PlatformOpenAI,
		Credentials: map[string]any{"chatgpt_account_id": uuid.NewString()}, Extra: map[string]any{
			"codex_7d_reset_at": f.reset.Format(time.RFC3339), "codex_7d_used_percent": 20.0, "codex_usage_updated_at": f.now.Format(time.RFC3339)}})
	for range 4 {
		u := mustCreateUser(t, client, &service.User{Balance: 10000})
		f.users = append(f.users, u)
		f.keys = append(f.keys, mustCreateApiKey(t, client, &service.APIKey{UserID: u.ID, Key: "sk-quota-" + uuid.NewString(), Quota: 10000}))
	}
	return f
}

func (f *quotaFixture) command(index int, actual, stats float64, ticket *service.AccountUserQuotaReservation) *service.UsageBillingCommand {
	return &service.UsageBillingCommand{RequestID: uuid.NewString(), UserID: f.users[index].ID, APIKeyID: f.keys[index].ID,
		AccountID: f.account.ID, AccountType: service.AccountTypeOAuth, BalanceCost: actual, APIKeyQuotaCost: actual,
		AccountUserQuotaTracking: true, AccountUserActualCost: actual, AccountUserStatsCost: stats,
		AccountUserExecutionStartedAt: f.now.Add(-10 * time.Minute), AccountUserQuotaReservation: ticket}
}

func (f *quotaFixture) save(version int64, members ...int) *service.AccountUserQuotaSnapshot {
	f.t.Helper()
	ids := make([]int64, 0, len(members))
	for _, i := range members {
		ids = append(ids, f.users[i].ID)
	}
	s, err := f.repo.Save(f.ctx, f.account.ID, f.users[0].ID, &service.SaveAccountUserQuotaInput{Enabled: true,
		ExpectedVersion: version, MemberUserIDs: ids, CarryoverSeconds: 1800}, f.now)
	require.NoError(f.t, err)
	return s
}

func (f *quotaFixture) ready(actual, stats float64) *service.AccountUserQuotaSnapshot {
	f.t.Helper()
	_, err := f.billing.Apply(f.ctx, f.command(0, actual, stats, nil))
	require.NoError(f.t, err)
	s := f.save(0, 0, 1, 2)
	require.NotNil(f.t, s.Cycle)
	s, err = f.repo.Initialize(f.ctx, f.account.ID, f.users[0].ID, s.Policy.Version, s.Cycle.ID, "已核对当前周期完整账单", f.now)
	require.NoError(f.t, err)
	return s
}

func (f *quotaFixture) observe(percent float64, observed, reset time.Time) *service.AccountUserQuotaSnapshot {
	f.t.Helper()
	extra := map[string]any{"codex_7d_reset_at": reset.Format(time.RFC3339), "codex_7d_used_percent": percent, "codex_usage_updated_at": observed.Format(time.RFC3339)}
	b, err := json.Marshal(extra)
	require.NoError(f.t, err)
	_, err = integrationDB.ExecContext(f.ctx, `UPDATE accounts SET extra=extra || $2::jsonb WHERE id=$1`, f.account.ID, string(b))
	require.NoError(f.t, err)
	s, err := f.repo.Get(f.ctx, f.account.ID, observed)
	require.NoError(f.t, err)
	return s
}

func (f *quotaFixture) admit(index int, amount float64, now time.Time) (*service.AccountUserQuotaReservation, error) {
	return f.repo.Admit(f.ctx, &service.AccountUserQuotaAdmission{AccountID: f.account.ID, UserID: f.users[index].ID,
		APIKeyID: f.keys[index].ID, ReservedCost: amount, PricingAt: now, RequestID: uuid.NewString()}, now)
}

func TestAccountUserQuotaRepositoryRealtimeReceiptsAndMembership(t *testing.T) {
	f := newQuotaFixture(t)
	s := f.ready(450, 300)
	require.Equal(t, "current", s.EstimateState)
	require.Equal(t, 500.0, s.Members[0].Quota)
	require.Equal(t, 450.0, s.Members[0].ActualCost, "durable receipt survives absent usage log")
	require.Equal(t, 50.0, s.Members[0].Remaining)
	cycle := s.Cycle.ID
	s = f.observe(25, f.now.Add(time.Second), f.reset)
	require.Equal(t, 400.0, s.Members[0].Quota)
	require.Error(t, s.Check(f.users[0].ID, 1, f.now.Add(time.Second)))
	require.NoError(t, s.Check(f.users[1].ID, 1, f.now.Add(time.Second)))
	s = f.observe(100.0/6, f.now.Add(2*time.Second), f.reset)
	require.Equal(t, 600.0, s.Members[0].Quota)
	require.NoError(t, s.Check(f.users[0].ID, 1, f.now.Add(2*time.Second)))
	s = f.save(s.Policy.Version, 0, 1, 2, 3)
	require.Equal(t, 450.0, s.Members[0].Quota)
	s = f.save(s.Policy.Version, 1, 2)
	s = f.save(s.Policy.Version, 0, 1, 2)
	require.Equal(t, 450.0, s.Members[0].ActualCost)
	require.Equal(t, cycle, s.Cycle.ID)
	s, err := f.repo.Initialize(f.ctx, f.account.ID, f.users[0].ID, s.Policy.Version, cycle, "重新核对完整周期账单", f.now.Add(3*time.Second))
	require.NoError(t, err)
	require.Equal(t, 450.0, s.Members[0].ActualCost, "initialization must not duplicate receipts")
	_, err = f.repo.Save(f.ctx, f.account.ID, f.users[0].ID, &service.SaveAccountUserQuotaInput{Enabled: true, ExpectedVersion: 1,
		MemberUserIDs: []int64{f.users[0].ID}, CarryoverSeconds: 1800}, f.now)
	require.ErrorIs(t, err, service.ErrAccountUserQuotaConfigConflict)
}

func TestAccountUserQuotaRepositoryConcurrentAdmissionAndSettlement(t *testing.T) {
	f := newQuotaFixture(t)
	s := f.ready(490, 300)
	var wg sync.WaitGroup
	tickets := make([]*service.AccountUserQuotaReservation, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			repo := NewAccountUserQuotaRepository(testEntClient(t), integrationDB)
			tickets[i], errs[i] = repo.Admit(f.ctx, &service.AccountUserQuotaAdmission{AccountID: f.account.ID, UserID: f.users[0].ID,
				APIKeyID: f.keys[0].ID, ReservedCost: 8, PricingAt: f.now}, f.now)
		}(i)
	}
	wg.Wait()
	var ticket *service.AccountUserQuotaReservation
	failures := 0
	for i := range 2 {
		if errs[i] != nil {
			failures++
			var q *service.AccountUserQuotaError
			require.ErrorAs(t, errs[i], &q)
			require.Equal(t, service.AccountUserQuotaInflight, q.Code)
		} else {
			ticket = tickets[i]
		}
	}
	require.Equal(t, 1, failures)
	require.NotNil(t, ticket)
	require.NoError(t, f.repo.ReleaseUnsent(f.ctx, ticket.Token, f.now))
	ticket, err := f.admit(0, 8, f.now)
	require.NoError(t, err)
	require.NoError(t, f.repo.MarkSent(f.ctx, ticket.Token, f.now))
	cmd := f.command(0, 9, 6, ticket)
	wrong := *cmd
	wrong.APIKeyID = f.keys[1].ID
	wrong.UserID = f.users[1].ID
	_, err = f.billing.Apply(f.ctx, &wrong)
	require.ErrorIs(t, err, service.ErrAccountUserQuotaReservationConflict)
	var balance float64
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT balance FROM users WHERE id=$1`, f.users[1].ID).Scan(&balance))
	require.Equal(t, 10000.0, balance)
	result, err := f.billing.Apply(f.ctx, cmd)
	require.NoError(t, err)
	require.True(t, result.Applied)
	result, err = f.billing.Apply(f.ctx, cmd)
	require.NoError(t, err)
	require.False(t, result.Applied)
	s, err = f.repo.Get(f.ctx, f.account.ID, f.now)
	require.NoError(t, err)
	require.Equal(t, 499.0, s.Members[0].ActualCost)
	require.Zero(t, s.Members[0].ReservedCost)
	require.Equal(t, 306.0, s.Cycle.AccountStatsCost)
}

func TestAccountUserQuotaRepositoryUnknownAndLateBilling(t *testing.T) {
	f := newQuotaFixture(t)
	f.ready(450, 300)
	ticket, err := f.admit(0, 8, f.now)
	require.NoError(t, err)
	require.NoError(t, f.repo.MarkSent(f.ctx, ticket.Token, f.now))
	require.NoError(t, f.repo.MarkUncertain(f.ctx, ticket.Token, f.now))
	s, err := f.repo.Get(f.ctx, f.account.ID, f.now.Add(16*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 8.0, s.Members[0].ReservedCost)
	require.Equal(t, 1, s.Uncertain)
	require.NoError(t, f.repo.ReconcileNoCharge(f.ctx, f.account.ID, ticket.Token, f.users[0].ID, "已核对上游及扣费收据未产生消费", f.now.Add(16*time.Minute)))
	_, err = f.billing.Apply(f.ctx, f.command(0, 3, 2, ticket))
	require.NoError(t, err)
	s, err = f.repo.Get(f.ctx, f.account.ID, f.now.Add(16*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 453.0, s.Members[0].ActualCost)
	require.Zero(t, s.Members[0].ReservedCost)
}

func TestAccountUserQuotaRepositoryCycleAndOutOfOrderObservation(t *testing.T) {
	f := newQuotaFixture(t)
	s := f.ready(450, 300)
	oldID := s.Cycle.ID
	ticket, err := f.admit(0, 8, f.now)
	require.NoError(t, err)
	require.NoError(t, f.repo.MarkSent(f.ctx, ticket.Token, f.now))
	s = f.observe(20, f.now.Add(time.Second), f.reset.Add(2*time.Second))
	require.Equal(t, oldID, s.Cycle.ID)
	version := s.Cycle.Version
	s = f.observe(10, f.now, f.reset)
	require.Equal(t, version, s.Cycle.Version)
	require.Nil(t, s.Cycle.MissingSince)
	s = f.observe(0, f.reset.Add(time.Second), f.reset.Add(7*24*time.Hour))
	require.NotEqual(t, oldID, s.Cycle.ID)
	require.Equal(t, "carryover", s.EstimateState)
	require.Zero(t, s.Members[0].ActualCost)
	require.Zero(t, s.Members[0].ReservedCost)
	_, err = f.billing.Apply(f.ctx, f.command(0, 3, 2, ticket))
	require.NoError(t, err)
	var actual, reserved float64
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT actual_cost,reserved_cost FROM account_user_quota_usage WHERE cycle_id=$1 AND user_id=$2`, oldID, f.users[0].ID).Scan(&actual, &reserved))
	require.Equal(t, 453.0, actual)
	require.Zero(t, reserved)
	_, err = f.repo.Get(f.ctx, f.account.ID, f.reset.Add(31*time.Minute))
	require.NoError(t, err)
}

func TestAccountUserQuotaRepositoryConfirmedEarlyResetAndIdentity(t *testing.T) {
	f := newQuotaFixture(t)
	s := f.ready(450, 300)
	oldID := s.Cycle.ID
	newReset := f.now.Add(7 * 24 * time.Hour)
	s = f.observe(0, f.now.Add(time.Second), newReset)
	require.Equal(t, oldID, s.Cycle.ID, "timestamp change alone must not reset")
	marker := map[string]any{"account_user_quota_confirmed_reset": map[string]any{"previous_reset_at": f.reset.Format(time.RFC3339), "confirmed_at": f.now.Add(time.Second).Format(time.RFC3339)}}
	b, err := json.Marshal(marker)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(f.ctx, `UPDATE accounts SET extra=extra || $2::jsonb WHERE id=$1`, f.account.ID, string(b))
	require.NoError(t, err)
	s = f.observe(0, f.now.Add(2*time.Second), newReset)
	require.NotEqual(t, oldID, s.Cycle.ID)
	require.Zero(t, s.Members[0].ActualCost)
	newID := s.Cycle.ID
	s = f.observe(0, f.now.Add(3*time.Second), newReset)
	require.Equal(t, newID, s.Cycle.ID)
	_, err = integrationDB.ExecContext(f.ctx, `UPDATE accounts SET credentials=jsonb_set(credentials,'{chatgpt_account_id}',to_jsonb($2::text)) WHERE id=$1`, f.account.ID, uuid.NewString())
	require.Error(t, err)
	alias := f.createAccount(&service.Account{Name: "alias-" + uuid.NewString(), Platform: service.PlatformOpenAI, Credentials: f.account.Credentials})
	_, err = f.repo.Get(f.ctx, alias.ID, f.now)
	require.Error(t, err, "duplicate account row cannot bypass quota")
}
