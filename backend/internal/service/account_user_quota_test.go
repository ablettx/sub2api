//go:build unit

package service

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func quotaTestSnapshot(now time.Time, estimate float64, users ...int64) *AccountUserQuotaSnapshot {
	id := int64(9)
	return &AccountUserQuotaSnapshot{
		Policy: &AccountUserQuotaPolicy{Enabled: true, MemberUserIDs: users, CarryoverSeconds: 1800},
		Cycle: &AccountUserQuotaCycle{ID: id, Ready: true, StartsAt: now.Add(-time.Hour), ResetsAt: now.Add(6 * 24 * time.Hour),
			Estimate: &estimate, EstimateAt: &now, EstimateSourceCycleID: &id},
	}
}

func TestAccountUserQuotaRealtimeAllocationPreservesActualUsage(t *testing.T) {
	now := time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)
	snapshot := quotaTestSnapshot(now, 1500, 1, 2, 3)
	snapshot.Members = []AccountUserQuotaUsage{{UserID: 1, ActualCost: 450}}
	for _, step := range []struct {
		estimate  float64
		users     []int64
		quota     float64
		remaining float64
		exhausted bool
	}{
		{1500, []int64{1, 2, 3}, 500, 50, false},
		{1200, []int64{1, 2, 3}, 400, 0, true},
		{1800, []int64{1, 2, 3}, 600, 150, false},
		{1800, []int64{1, 2, 3, 4}, 450, 0, true},
		{1800, []int64{1, 2, 3}, 600, 150, false},
	} {
		snapshot.Cycle.Estimate = &step.estimate
		snapshot.Policy.MemberUserIDs = step.users
		err := snapshot.Check(1, 1, now)
		if step.exhausted {
			var quotaErr *AccountUserQuotaError
			require.ErrorAs(t, err, &quotaErr)
			require.Equal(t, AccountUserQuotaExhausted, quotaErr.Code)
		} else {
			require.NoError(t, err)
		}
		require.Equal(t, step.quota, snapshot.Members[0].Quota)
		require.Equal(t, float64(450), snapshot.Members[0].ActualCost)
		require.Equal(t, step.remaining, snapshot.Members[0].Remaining)
		require.NoError(t, snapshot.Check(2, 1, now), "one user's exhaustion must not block another")
	}
}

func TestAccountUserQuotaReservationAndPrecision(t *testing.T) {
	now := time.Now().UTC()
	snapshot := quotaTestSnapshot(now, 30, 1, 2, 3)
	snapshot.Members = []AccountUserQuotaUsage{{UserID: 1, ReservedCost: 8}}
	var quotaErr *AccountUserQuotaError
	require.ErrorAs(t, snapshot.Check(1, 8, now), &quotaErr)
	require.Equal(t, AccountUserQuotaInflight, quotaErr.Code)
	require.NoError(t, snapshot.Check(1, 2, now))
	require.Equal(t, float64(10), snapshot.Members[0].Remaining)
	require.Equal(t, float64(2), snapshot.Members[0].Available)
	require.Equal(t, 0.33333333, AccountUserQuotaShare(1, 3))
	require.Less(t, AccountUserQuotaShare(1, 3)*3, float64(1))
	snapshot = quotaTestSnapshot(now, 0.00000003, 1)
	snapshot.Members = []AccountUserQuotaUsage{{UserID: 1, ActualCost: 0.00000002}}
	require.NoError(t, snapshot.Check(1, 0.00000001, now), "visible zero must not erase a valid monetary unit")
}

func TestAccountUserQuotaEstimateUsesAccountStatisticsOnly(t *testing.T) {
	estimate, ok := AccountUserQuotaEstimate(20, 2)
	require.True(t, ok)
	require.Equal(t, float64(1000), estimate)
	for _, invalid := range []float64{-1, 0, math.Inf(1), math.NaN()} {
		_, ok := AccountUserQuotaEstimate(invalid, 2)
		require.False(t, ok)
		_, ok = AccountUserQuotaEstimate(20, invalid)
		require.False(t, ok)
	}
	_, ok = AccountUserQuotaEstimate(20, 101)
	require.False(t, ok)
}

func TestAccountUserQuotaCarryoverDoesNotRefreshByReading(t *testing.T) {
	now := time.Now().UTC()
	snapshot := quotaTestSnapshot(now, 1500, 1, 2, 3)
	missing := now.Add(-20 * time.Minute)
	oldCycle := int64(8)
	snapshot.Cycle.MissingSince = &missing
	snapshot.Cycle.EstimateSourceCycleID = &oldCycle
	require.NoError(t, snapshot.Check(1, 1, now))
	require.Equal(t, "carryover", snapshot.EstimateState)
	expires := *snapshot.ExpiresAt
	require.NoError(t, snapshot.Check(1, 1, now.Add(9*time.Minute)))
	require.Equal(t, expires, *snapshot.ExpiresAt)
	var quotaErr *AccountUserQuotaError
	require.ErrorAs(t, snapshot.Check(1, 1, now.Add(10*time.Minute)), &quotaErr)
	require.Equal(t, AccountUserQuotaEstimateUnavailable, quotaErr.Code)
	require.Equal(t, missing, *snapshot.Cycle.MissingSince)
}

func TestAccountUserQuotaMissingInitializationAndMembership(t *testing.T) {
	now := time.Now().UTC()
	snapshot := quotaTestSnapshot(now, 1500, 1, 2, 3)
	var quotaErr *AccountUserQuotaError
	require.ErrorAs(t, snapshot.Check(99, 1, now), &quotaErr)
	require.Equal(t, AccountUserQuotaNotMember, quotaErr.Code)
	snapshot.Cycle.Ready = false
	require.ErrorAs(t, snapshot.Check(1, 0, now), &quotaErr)
	require.Equal(t, AccountUserQuotaInitializationRequired, quotaErr.Code)
	snapshot.Policy.Enabled = false
	require.NoError(t, snapshot.Check(99, math.Inf(1), now), "disabled quota does not add restrictions")
}

func TestAccountUserQuotaObservationPreservesSourceAndWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(6 * 24 * time.Hour)
	extra := map[string]any{"codex_usage_updated_at": now.Format(time.RFC3339), "codex_7d_reset_at": reset.Format(time.RFC3339), "codex_7d_used_percent": 12.5}
	observation := AccountUserQuotaObservationFromExtra(extra, now.Add(time.Minute))
	require.NotNil(t, observation)
	require.Equal(t, now, observation.ObservedAt)
	require.Equal(t, reset, observation.ResetsAt)
	require.Equal(t, 12.5, *observation.UsedPercent)
	require.True(t, AccountUserQuotaSameCycle(reset, reset.Add(2*time.Second)))
	require.False(t, AccountUserQuotaSameCycle(reset, reset.Add(24*time.Hour)))
	delete(extra, "codex_7d_reset_at")
	extra["codex_7d_reset_after_seconds"] = int((6 * 24 * time.Hour).Seconds())
	require.Equal(t, reset, AccountUserQuotaObservationFromExtra(extra, now.Add(time.Minute)).ResetsAt)
	extra["codex_usage_updated_at"] = now.Add(time.Hour).Format(time.RFC3339)
	require.Nil(t, AccountUserQuotaObservationFromExtra(extra, now))
}

func TestAccountUserQuotaConfigurationRejectsAmbiguousInputs(t *testing.T) {
	for _, in := range []SaveAccountUserQuotaInput{
		{Enabled: true, CarryoverSeconds: 1800},
		{MemberUserIDs: []int64{1, 1}, CarryoverSeconds: 1800},
		{MemberUserIDs: []int64{-1}, CarryoverSeconds: 1800},
		{MemberUserIDs: []int64{1}, CarryoverSeconds: 0},
		{ExpectedVersion: -1, CarryoverSeconds: 1800},
		{FallbackGroupIDs: []int64{4, 4}, CarryoverSeconds: 1800},
	} {
		require.Error(t, in.Validate())
	}
	require.NoError(t, (&SaveAccountUserQuotaInput{Enabled: true, MemberUserIDs: []int64{1, 2, 3}, CarryoverSeconds: 1800}).Validate())
}
