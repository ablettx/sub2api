package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

type accountUserQuotaRepository struct{ db *sql.DB }

func NewAccountUserQuotaRepository(_ *dbent.Client, db *sql.DB) service.AccountUserQuotaRepository {
	return &accountUserQuotaRepository{db: db}
}

func (r *accountUserQuotaRepository) transact(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, service.AccountUserQuotaOperationTimeout)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = fn(ctx, tx)
		if err == nil {
			err = tx.Commit()
		}
		_ = tx.Rollback()
		var pgErr *pq.Error
		if err == nil || !errors.As(err, &pgErr) || (pgErr.Code != "40001" && pgErr.Code != "40P01") {
			return err
		}
	}
	return errors.New("account quota transaction contention exceeded retry budget")
}

func (r *accountUserQuotaRepository) Get(ctx context.Context, accountID int64, now time.Time) (*service.AccountUserQuotaSnapshot, error) {
	var snapshot *service.AccountUserQuotaSnapshot
	err := r.transact(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		snapshot, err = loadAccountUserQuota(ctx, tx, accountID, now)
		return err
	})
	return snapshot, err
}

func lockAccountUserQuotaPolicy(ctx context.Context, tx *sql.Tx, accountID int64) (*service.AccountUserQuotaPolicy, error) {
	p := &service.AccountUserQuotaPolicy{AccountID: accountID}
	var groups []byte
	err := tx.QueryRowContext(ctx, `SELECT enabled, config_version, fallback_group_ids, carryover_seconds,
		history_note, history_verified_at, enforced_at, upstream_account_id FROM account_user_quota_policies WHERE account_id=$1 FOR UPDATE`, accountID).
		Scan(&p.Enabled, &p.Version, &groups, &p.CarryoverSeconds, &p.HistoryNote, &p.HistoryVerifiedAt, &p.EnforcedAt, &p.UpstreamAccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(groups, &p.FallbackGroupIDs); err != nil {
		return nil, fmt.Errorf("decode quota fallback groups: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.user_id FROM account_user_quota_members m
		JOIN users u ON u.id=m.user_id
		WHERE m.account_id=$1 AND m.active ORDER BY m.user_id`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	p.MemberUserIDs = []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		p.MemberUserIDs = append(p.MemberUserIDs, id)
	}
	return p, rows.Err()
}

const accountUserQuotaCycleColumns = `id,account_id,starts_at,resets_at,ready,version,account_stats_cost,
	estimate,estimate_at,estimate_source_cycle_id,observed_at,used_percent,missing_since,backfill_through_id`

func scanAccountUserQuotaCycle(row interface{ Scan(...any) error }) (*service.AccountUserQuotaCycle, error) {
	c := &service.AccountUserQuotaCycle{}
	err := row.Scan(&c.ID, &c.AccountID, &c.StartsAt, &c.ResetsAt, &c.Ready, &c.Version, &c.AccountStatsCost,
		&c.Estimate, &c.EstimateAt, &c.EstimateSourceCycleID, &c.ObservedAt, &c.UsedPercent, &c.MissingSince, &c.BackfillThroughID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func loadAccountUserQuota(ctx context.Context, tx *sql.Tx, accountID int64, now time.Time) (*service.AccountUserQuotaSnapshot, error) {
	p, err := lockAccountUserQuotaPolicy(ctx, tx, accountID)
	if err != nil {
		return nil, err
	}
	snapshot := &service.AccountUserQuotaSnapshot{Policy: p, Members: []service.AccountUserQuotaUsage{}}
	if p == nil {
		// Another database row must not bypass a controlled upstream identity.
		var ownerID int64
		err := tx.QueryRowContext(ctx, `SELECT p.account_id FROM account_user_quota_policies p
			JOIN accounts requested ON requested.id=$1 AND requested.platform='openai' AND requested.type='oauth' AND requested.parent_account_id IS NULL
			WHERE p.enabled AND p.upstream_account_id<>'' AND p.upstream_account_id=requested.credentials->>'chatgpt_account_id' LIMIT 1`, accountID).Scan(&ownerID)
		if err == nil {
			return nil, service.NewAccountUserQuotaError(service.AccountUserQuotaUnavailable, "This account duplicates a quota-controlled account; use the configured quota owner")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return snapshot, nil
	}
	cycle, err := synchronizeAccountUserQuotaCycle(ctx, tx, p, now)
	if err != nil {
		return nil, err
	}
	snapshot.Cycle = cycle
	if cycle != nil {
		rows, err := tx.QueryContext(ctx, `SELECT user_id,actual_cost,reserved_cost FROM account_user_quota_usage WHERE cycle_id=$1 ORDER BY user_id`, cycle.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var usage service.AccountUserQuotaUsage
			if err := rows.Scan(&usage.UserID, &usage.ActualCost, &usage.ReservedCost); err != nil {
				_ = rows.Close()
				return nil, err
			}
			snapshot.Members = append(snapshot.Members, usage)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_user_quota_reservations
			WHERE account_id=$1 AND (state='uncertain' OR (state IN ('reserved','sent') AND lease_until <= $2))`, accountID, now).
			Scan(&snapshot.Uncertain); err != nil {
			return nil, err
		}
	}
	snapshot.Populate(now)
	return snapshot, nil
}

func synchronizeAccountUserQuotaCycle(ctx context.Context, tx *sql.Tx, p *service.AccountUserQuotaPolicy, now time.Time) (*service.AccountUserQuotaCycle, error) {
	c, err := scanAccountUserQuotaCycle(tx.QueryRowContext(ctx, `SELECT `+accountUserQuotaCycleColumns+`
		FROM account_user_quota_cycles WHERE account_id=$1 ORDER BY resets_at DESC LIMIT 1 FOR UPDATE`, p.AccountID))
	if err != nil {
		return nil, err
	}
	var extraJSON []byte
	var upstreamID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(extra,'{}'::jsonb),credentials->>'chatgpt_account_id' FROM accounts WHERE id=$1 AND deleted_at IS NULL`, p.AccountID).Scan(&extraJSON, &upstreamID); err != nil {
		return nil, err
	}
	if p.Enabled && (p.UpstreamAccountID == "" || p.UpstreamAccountID != upstreamID.String) {
		return nil, service.NewAccountUserQuotaError(service.AccountUserQuotaUnavailable, "Controlled upstream account identity changed; verification is required")
	}
	var extra map[string]any
	if err := json.Unmarshal(extraJSON, &extra); err != nil {
		return nil, fmt.Errorf("decode quota observation: %w", err)
	}
	observation := service.AccountUserQuotaObservationFromExtra(extra, now)
	validWindow := observation != nil && now.Before(observation.ResetsAt) && !now.Before(observation.ResetsAt.Add(-service.AccountUserQuotaWindow))
	var earlyStart *time.Time
	if c != nil && validWindow && !service.AccountUserQuotaSameCycle(observation.ResetsAt, c.ResetsAt) {
		if marker, ok := extra["account_user_quota_confirmed_reset"].(map[string]any); ok {
			previous, previousErr := time.Parse(time.RFC3339Nano, fmt.Sprint(marker["previous_reset_at"]))
			confirmed, confirmedErr := time.Parse(time.RFC3339Nano, fmt.Sprint(marker["confirmed_at"]))
			if previousErr == nil && confirmedErr == nil && service.AccountUserQuotaSameCycle(previous, c.ResetsAt) &&
				confirmed.After(c.StartsAt) && confirmed.Before(c.ResetsAt) && !observation.ObservedAt.Before(confirmed) &&
				observation.ResetsAt.After(confirmed) {
				earlyStart = &confirmed
			}
		}
	}
	newWindow := validWindow && (c == nil || (!service.AccountUserQuotaSameCycle(observation.ResetsAt, c.ResetsAt) &&
		((!now.Before(c.ResetsAt) && !observation.ObservedAt.Before(c.ResetsAt)) || earlyStart != nil)))
	if newWindow {
		var previousEstimate *float64
		var previousAt *time.Time
		var previousID *int64
		if c != nil && c.Estimate != nil {
			previousEstimate, previousAt, previousID = c.Estimate, c.EstimateAt, c.EstimateSourceCycleID
		}
		start := observation.ResetsAt.Add(-service.AccountUserQuotaWindow)
		if earlyStart != nil {
			start = *earlyStart
		}
		c, err = scanAccountUserQuotaCycle(tx.QueryRowContext(ctx, `INSERT INTO account_user_quota_cycles
			(account_id,starts_at,resets_at,ready,estimate,estimate_at,estimate_source_cycle_id,missing_since)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$2) RETURNING `+accountUserQuotaCycleColumns,
			p.AccountID, start, observation.ResetsAt, p.HistoryVerifiedAt != nil, previousEstimate, previousAt, previousID))
		if err != nil {
			return nil, err
		}
	}
	if c == nil {
		return nil, nil
	}
	if observation != nil && c.ObservedAt != nil && observation.ObservedAt.Before(*c.ObservedAt) {
		// Ignore stale delivery without changing the established budget or freshness.
		return c, nil
	}
	if !validWindow || !service.AccountUserQuotaSameCycle(observation.ResetsAt, c.ResetsAt) {
		if c.MissingSince == nil {
			c.MissingSince = &now
			_, err = tx.ExecContext(ctx, `UPDATE account_user_quota_cycles SET missing_since=$2,version=version+1,updated_at=$2 WHERE id=$1`, c.ID, now)
		}
		return c, err
	}
	c.ObservedAt, c.UsedPercent = &observation.ObservedAt, observation.UsedPercent
	if observation.UsedPercent != nil && now.Sub(observation.ObservedAt) <= 10*time.Minute {
		if estimate, ok := service.AccountUserQuotaEstimate(c.AccountStatsCost, *observation.UsedPercent); ok {
			c.Estimate, c.EstimateAt, c.EstimateSourceCycleID, c.MissingSince = &estimate, &observation.ObservedAt, &c.ID, nil
		} else if c.MissingSince == nil {
			c.MissingSince = &now
		}
	} else if c.MissingSince == nil {
		missingSince := now
		if observation.ObservedAt.Add(10 * time.Minute).Before(now) {
			missingSince = observation.ObservedAt.Add(10 * time.Minute)
		}
		c.MissingSince = &missingSince
	}
	_, err = tx.ExecContext(ctx, `UPDATE account_user_quota_cycles SET estimate=$2,estimate_at=$3,
		estimate_source_cycle_id=$4,observed_at=$5,used_percent=$6,missing_since=$7,version=version+1,updated_at=$8 WHERE id=$1`,
		c.ID, c.Estimate, c.EstimateAt, c.EstimateSourceCycleID, c.ObservedAt, c.UsedPercent, c.MissingSince, now)
	c.Version++
	return c, err
}

func (r *accountUserQuotaRepository) Save(ctx context.Context, accountID, actorID int64, in *service.SaveAccountUserQuotaInput, now time.Time) (*service.AccountUserQuotaSnapshot, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	err := r.transact(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var platform, accountType string
		var credentials []byte
		var parentID *int64
		if err := tx.QueryRowContext(ctx, `SELECT platform,type,credentials,parent_account_id FROM accounts WHERE id=$1 AND deleted_at IS NULL`, accountID).
			Scan(&platform, &accountType, &credentials, &parentID); err != nil {
			return err
		}
		account := &service.Account{Platform: platform, Type: accountType}
		if err := json.Unmarshal(credentials, &account.Credentials); err != nil {
			return err
		}
		if platform != service.PlatformOpenAI || accountType != service.AccountTypeOAuth || parentID != nil || account.IsOpenAIPersonalAccessToken() || account.IsOpenAIAgentIdentity() {
			return service.NewAccountUserQuotaValidationError("dynamic account quota supports standard OpenAI OAuth accounts only")
		}
		upstreamID := account.GetChatGPTAccountID()
		if in.Enabled {
			if upstreamID == "" {
				return service.NewAccountUserQuotaValidationError("verify the upstream account identity before enabling quota")
			}
			var duplicate bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id<>$1 AND deleted_at IS NULL
				AND platform='openai' AND type='oauth' AND parent_account_id IS NULL AND credentials->>'chatgpt_account_id'=$2)`, accountID, upstreamID).Scan(&duplicate); err != nil {
				return err
			}
			if duplicate {
				return service.NewAccountUserQuotaValidationError("duplicate upstream account records require a single quota owner before enabling")
			}
		}
		var valid int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users u WHERE u.id=ANY($1)
			AND (NOT $3 OR (u.deleted_at IS NULL AND u.status='active') OR EXISTS(
				SELECT 1 FROM account_user_quota_members m WHERE m.account_id=$2 AND m.user_id=u.id AND m.active))`,
			pq.Array(in.MemberUserIDs), accountID, in.Enabled).Scan(&valid); err != nil {
			return err
		}
		if valid != len(in.MemberUserIDs) {
			return service.NewAccountUserQuotaValidationError("quota contains unavailable users")
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM groups WHERE id=ANY($1) AND deleted_at IS NULL AND status='active' AND platform='openai'`, pq.Array(in.FallbackGroupIDs)).Scan(&valid); err != nil {
			return err
		}
		if in.Enabled && valid != len(in.FallbackGroupIDs) {
			return service.NewAccountUserQuotaValidationError("fallback groups must be active OpenAI groups")
		}
		groups, err := json.Marshal(in.FallbackGroupIDs)
		if err != nil {
			return err
		}
		if in.FallbackGroupIDs == nil {
			groups = []byte("[]")
		}
		if in.ExpectedVersion == 0 {
			result, err := tx.ExecContext(ctx, `INSERT INTO account_user_quota_policies
				(account_id,enabled,fallback_group_ids,carryover_seconds,updated_by,created_at,updated_at,enforced_at,upstream_account_id)
				VALUES ($1,$2,$3,$4,$5,$6::timestamptz,$6::timestamptz,CASE WHEN $2 THEN $6::timestamptz ELSE NULL END,$7) ON CONFLICT(account_id) DO NOTHING`, accountID, in.Enabled, string(groups), in.CarryoverSeconds, actorID, now, upstreamID)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count == 0 {
				return service.ErrAccountUserQuotaConfigConflict
			}
		} else {
			result, err := tx.ExecContext(ctx, `UPDATE account_user_quota_policies SET enforced_at=CASE WHEN $2 AND NOT enabled THEN $6::timestamptz ELSE enforced_at END,enabled=$2,
				upstream_account_id=CASE WHEN upstream_account_id='' THEN $8 ELSE upstream_account_id END,
				fallback_group_ids=$3,carryover_seconds=$4,updated_by=$5,config_version=config_version+1,updated_at=$6
				WHERE account_id=$1 AND config_version=$7 AND (NOT $2 OR upstream_account_id='' OR upstream_account_id=$8)`, accountID, in.Enabled, string(groups), in.CarryoverSeconds, actorID, now, in.ExpectedVersion, upstreamID)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count == 0 {
				return service.ErrAccountUserQuotaConfigConflict
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_user_quota_members SET active=FALSE,updated_at=$2 WHERE account_id=$1 AND active`, accountID, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO account_user_quota_members(account_id,user_id,active,created_at,updated_at)
			SELECT $1,unnest($2::bigint[]),TRUE,$3,$3 ON CONFLICT(account_id,user_id) DO UPDATE SET active=TRUE,updated_at=$3`, accountID, pq.Array(in.MemberUserIDs), now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, accountID, now)
}

func (r *accountUserQuotaRepository) Initialize(ctx context.Context, accountID, actorID, expectedVersion, cycleID int64, note string, now time.Time) (*service.AccountUserQuotaSnapshot, error) {
	err := r.transact(ctx, func(ctx context.Context, tx *sql.Tx) error {
		snapshot, err := loadAccountUserQuota(ctx, tx, accountID, now)
		if err != nil {
			return err
		}
		if snapshot.Policy == nil || snapshot.Cycle == nil || !now.Before(snapshot.Cycle.ResetsAt) {
			return service.NewAccountUserQuotaError(service.AccountUserQuotaInitializationRequired, "A confirmed current account cycle is required")
		}
		if snapshot.Policy.Version != expectedVersion || snapshot.Cycle.ID != cycleID {
			return service.ErrAccountUserQuotaConfigConflict
		}
		if snapshot.Uncertain > 0 {
			return service.NewAccountUserQuotaError(service.AccountUserQuotaUnavailable, "Reconcile outstanding account reservations before initialization")
		}
		c := snapshot.Cycle
		var unresolvedReceipt bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM usage_billing_dedup WHERE account_id=$1 AND actual_cost IS NOT NULL
			AND account_user_quota_cycle_id IS NULL AND execution_started_at IS NULL AND created_at >= $2 AND created_at < $3
			UNION ALL SELECT 1 FROM usage_billing_dedup_archive WHERE account_id=$1 AND actual_cost IS NOT NULL
			AND account_user_quota_cycle_id IS NULL AND execution_started_at IS NULL AND created_at >= $2 AND created_at < $3)`,
			accountID, c.StartsAt, c.ResetsAt).Scan(&unresolvedReceipt); err != nil {
			return err
		}
		if unresolvedReceipt {
			return service.NewAccountUserQuotaError(service.AccountUserQuotaInitializationRequired, "Billing receipts without a confirmed execution period require reconciliation")
		}
		if err := backfillAccountUserQuotaReceipts(ctx, tx, c); err != nil {
			return err
		}
		var watermark int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM usage_logs WHERE account_id=$1`, accountID).Scan(&watermark); err != nil {
			return err
		}
		// Only unassociated historical rows are backfilled. New billing already owns a cycle.
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_user_quota_usage(cycle_id,user_id,actual_cost,reserved_cost)
			SELECT $1,ul.user_id,SUM(ROUND(ul.actual_cost::numeric,8)),0 FROM usage_logs ul
			WHERE ul.account_id=$2 AND ul.id>$3 AND ul.id<=$4 AND ul.created_at >= $5 AND ul.created_at < $6
			AND ul.account_user_quota_cycle_id IS NULL
			AND NOT EXISTS(SELECT 1 FROM usage_billing_dedup d WHERE d.request_id=ul.request_id AND d.api_key_id=ul.api_key_id AND d.actual_cost IS NOT NULL)
			AND NOT EXISTS(SELECT 1 FROM usage_billing_dedup_archive d WHERE d.request_id=ul.request_id AND d.api_key_id=ul.api_key_id AND d.actual_cost IS NOT NULL)
			GROUP BY ul.user_id
			ON CONFLICT(cycle_id,user_id) DO UPDATE SET actual_cost=account_user_quota_usage.actual_cost+EXCLUDED.actual_cost,updated_at=NOW()`,
			c.ID, accountID, c.BackfillThroughID, watermark, c.StartsAt, c.ResetsAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_user_quota_cycles SET account_stats_cost=account_stats_cost+(
			SELECT COALESCE(SUM(ROUND((COALESCE(ul.account_stats_cost,ul.total_cost)*COALESCE(ul.account_rate_multiplier,1))::numeric,8)),0) FROM usage_logs ul
			WHERE ul.account_id=$2 AND ul.id>$3 AND ul.id<=$4 AND ul.created_at >= $5 AND ul.created_at < $6 AND ul.account_user_quota_cycle_id IS NULL
			AND NOT EXISTS(SELECT 1 FROM usage_billing_dedup d WHERE d.request_id=ul.request_id AND d.api_key_id=ul.api_key_id AND d.actual_cost IS NOT NULL)
			AND NOT EXISTS(SELECT 1 FROM usage_billing_dedup_archive d WHERE d.request_id=ul.request_id AND d.api_key_id=ul.api_key_id AND d.actual_cost IS NOT NULL)),
			backfill_through_id=$4,ready=TRUE,version=version+1,updated_at=$7 WHERE id=$1`,
			c.ID, accountID, c.BackfillThroughID, watermark, c.StartsAt, c.ResetsAt, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE account_user_quota_policies SET history_note=$2,history_verified_at=$3,
			updated_by=$4,updated_at=$3 WHERE account_id=$1`, accountID, note, now, actorID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, accountID, now)
}

func (r *accountUserQuotaRepository) Admit(ctx context.Context, in *service.AccountUserQuotaAdmission, now time.Time) (*service.AccountUserQuotaReservation, error) {
	if in == nil || in.AccountID <= 0 || in.UserID <= 0 || in.APIKeyID <= 0 || in.ReservedCost < 0 || math.IsNaN(in.ReservedCost) || math.IsInf(in.ReservedCost, 0) {
		return nil, errors.New("invalid account quota admission")
	}
	var reservation *service.AccountUserQuotaReservation
	err := r.transact(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var userActive bool
		if err := tx.QueryRowContext(ctx, `SELECT status='active' AND deleted_at IS NULL FROM users WHERE id=$1 FOR SHARE`, in.UserID).Scan(&userActive); err != nil {
			return err
		}
		if !userActive {
			return service.NewAccountUserQuotaError(service.AccountUserQuotaUnavailable, "User access is no longer active")
		}
		snapshot, err := loadAccountUserQuota(ctx, tx, in.AccountID, now)
		if err != nil {
			return err
		}
		if snapshot.Policy == nil || !snapshot.Policy.Enabled {
			return nil
		}
		if in.Unmetered {
			return service.NewAccountUserQuotaError(service.AccountUserQuotaUnmetered, "This endpoint cannot meter a controlled account; use another authorized account")
		}
		amount := service.QuantizeUsageBillingAmount(in.ReservedCost)
		if in.Calibration == nil {
			if err := snapshot.Check(in.UserID, amount, now); err != nil {
				return err
			}
		} else if err := checkAccountUserQuotaCalibration(ctx, tx, snapshot, in, amount, now); err != nil {
			return err
		}
		reservation = &service.AccountUserQuotaReservation{
			Token: uuid.NewString(), AccountID: in.AccountID, CycleID: snapshot.Cycle.ID, UserID: in.UserID, APIKeyID: in.APIKeyID,
			OriginalGroupID: in.OriginalGroupID, EffectiveGroupID: in.EffectiveGroupID, SubscriptionID: in.SubscriptionID,
			PricingAt: in.PricingAt, ConfigVersion: snapshot.Policy.Version, ReservedCost: amount,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_user_quota_usage(cycle_id,user_id,reserved_cost)
			VALUES ($1,$2,$3) ON CONFLICT(cycle_id,user_id) DO UPDATE
			SET reserved_cost=account_user_quota_usage.reserved_cost+$3,updated_at=$4`, reservation.CycleID, in.UserID, amount, now); err != nil {
			return err
		}
		var calibrationRun *string
		var calibrationOrdinal, calibrationMax *int
		var calibrationBudget *float64
		if in.Calibration != nil {
			calibrationRun, calibrationOrdinal, calibrationMax, calibrationBudget = &in.Calibration.RunID, &in.Calibration.Ordinal, &in.Calibration.MaxRequests, &in.Calibration.Budget
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO account_user_quota_reservations
			(token,account_id,cycle_id,user_id,api_key_id,original_group_id,effective_group_id,subscription_id,
			request_id,pricing_at,config_version,reserved_cost,state,lease_until,created_at,updated_at,
			calibration_run_id,calibration_ordinal,calibration_max_requests,calibration_budget)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'reserved',$13,$14,$14,$15,$16,$17,$18)`,
			reservation.Token, in.AccountID, reservation.CycleID, in.UserID, in.APIKeyID, in.OriginalGroupID,
			in.EffectiveGroupID, in.SubscriptionID, in.RequestID, in.PricingAt, reservation.ConfigVersion, amount, now.Add(service.AccountUserQuotaLease), now,
			calibrationRun, calibrationOrdinal, calibrationMax, calibrationBudget)
		return err
	})
	return reservation, err
}

func checkAccountUserQuotaCalibration(ctx context.Context, tx *sql.Tx, snapshot *service.AccountUserQuotaSnapshot, in *service.AccountUserQuotaAdmission, amount float64, now time.Time) error {
	calibration := in.Calibration
	if _, err := uuid.Parse(calibration.RunID); err != nil || calibration.Ordinal < 1 || calibration.Ordinal > 3 ||
		calibration.MaxRequests < calibration.Ordinal || calibration.MaxRequests > 3 ||
		!service.AccountUserQuotaBudgetFits(calibration.Budget, 0, amount) {
		return errors.New("invalid or insufficient calibration budget")
	}
	if snapshot.Cycle == nil || !snapshot.Cycle.Ready || snapshot.Cycle.ID != calibration.CycleID || !now.Before(snapshot.Cycle.ResetsAt) || snapshot.Uncertain > 0 {
		return service.NewAccountUserQuotaError(service.AccountUserQuotaInitializationRequired, "Verify current cycle history and pending reservations before calibration")
	}
	member := false
	for _, userID := range snapshot.Policy.MemberUserIDs {
		member = member || userID == in.UserID
	}
	if !member {
		return service.NewAccountUserQuotaError(service.AccountUserQuotaNotMember, "Calibration user must be a quota participant")
	}
	if snapshot.EstimateState == "current" || snapshot.EstimateState == "carryover" {
		if err := snapshot.Check(in.UserID, amount, now); err != nil {
			return err
		}
	}
	var used float64
	var count int
	var budget sql.NullFloat64
	var maximum sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN state='settled' THEN actual_cost
		WHEN state IN ('reserved','sent','uncertain') THEN reserved_cost ELSE 0 END),0),COUNT(*),MIN(calibration_budget),MAX(calibration_max_requests)
		FROM account_user_quota_reservations WHERE calibration_run_id=$1`, calibration.RunID).Scan(&used, &count, &budget, &maximum); err != nil {
		return err
	}
	if count != calibration.Ordinal-1 || (budget.Valid && budget.Float64 != calibration.Budget) || (maximum.Valid && maximum.Int64 != int64(calibration.MaxRequests)) ||
		!service.AccountUserQuotaBudgetFits(calibration.Budget, used, amount) {
		return service.NewAccountUserQuotaError(service.AccountUserQuotaInflight, "Calibration budget or request count has been exhausted")
	}
	return nil
}

func (r *accountUserQuotaRepository) MarkSent(ctx context.Context, token string, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, service.AccountUserQuotaOperationTimeout)
	defer cancel()
	result, err := r.db.ExecContext(ctx, `UPDATE account_user_quota_reservations SET state='sent',updated_at=$2
		WHERE token=$1 AND state='reserved' AND lease_until>$2`, token, now)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return service.ErrAccountUserQuotaReservationConflict
	}
	return err
}

func (r *accountUserQuotaRepository) Renew(ctx context.Context, token string, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, service.AccountUserQuotaOperationTimeout)
	defer cancel()
	_, err := r.db.ExecContext(ctx, `UPDATE account_user_quota_reservations SET lease_until=$2,updated_at=$3
		WHERE token=$1 AND state IN ('reserved','sent')`, token, now.Add(service.AccountUserQuotaLease), now)
	return err
}

func (r *accountUserQuotaRepository) MarkUncertain(ctx context.Context, token string, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, service.AccountUserQuotaOperationTimeout)
	defer cancel()
	_, err := r.db.ExecContext(ctx, `UPDATE account_user_quota_reservations SET state='uncertain',updated_at=$2
		WHERE token=$1 AND state IN ('reserved','sent')`, token, now)
	return err
}

func (r *accountUserQuotaRepository) ReleaseUnsent(ctx context.Context, token string, now time.Time) error {
	return r.transact(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var accountID int64
		if err := tx.QueryRowContext(ctx, `SELECT account_id FROM account_user_quota_reservations WHERE token=$1`, token).Scan(&accountID); err != nil {
			return err
		}
		if _, err := lockAccountUserQuotaPolicy(ctx, tx, accountID); err != nil {
			return err
		}
		var cycleID, userID int64
		var amount float64
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT cycle_id,user_id,reserved_cost,state FROM account_user_quota_reservations WHERE token=$1 FOR UPDATE`, token).
			Scan(&cycleID, &userID, &amount, &state); err != nil {
			return err
		}
		if state != "reserved" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_user_quota_usage SET reserved_cost=reserved_cost-$3,updated_at=$4 WHERE cycle_id=$1 AND user_id=$2`, cycleID, userID, amount, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE account_user_quota_reservations SET state='released',updated_at=$2 WHERE token=$1`, token, now)
		return err
	})
}
