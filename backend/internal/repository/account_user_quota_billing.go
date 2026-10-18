package repository

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// applyAccountUserQuotaBilling participates in the existing money transaction.
// The policy lock serializes admission, configuration, backfill and settlement.
func applyAccountUserQuotaBilling(ctx context.Context, tx *sql.Tx, cmd *service.UsageBillingCommand, result *service.UsageBillingApplyResult) error {
	if !cmd.AccountUserQuotaTracking && cmd.AccountUserQuotaReservation == nil {
		return nil
	}
	if cmd.AccountUserActualCost < 0 || cmd.AccountUserStatsCost < 0 ||
		math.IsNaN(cmd.AccountUserActualCost) || math.IsNaN(cmd.AccountUserStatsCost) ||
		math.IsInf(cmd.AccountUserActualCost, 0) || math.IsInf(cmd.AccountUserStatsCost, 0) {
		return errors.New("invalid account quota billing amounts")
	}
	p, err := lockAccountUserQuotaPolicy(ctx, tx, cmd.AccountID)
	if err != nil {
		return err
	}
	reservation := cmd.AccountUserQuotaReservation
	if p == nil {
		if reservation != nil {
			return service.ErrAccountUserQuotaReservationConflict
		}
		return writeAccountUserQuotaBillingReceipt(ctx, tx, cmd, nil)
	}
	now := time.Now().UTC()
	var cycleID int64
	var reserved float64
	if reservation != nil {
		var accountID, userID, keyID int64
		var state string
		var reconciliation string
		err := tx.QueryRowContext(ctx, `SELECT account_id,cycle_id,user_id,api_key_id,reserved_cost,state,reconciliation_note
			FROM account_user_quota_reservations WHERE token=$1 FOR UPDATE`, reservation.Token).
			Scan(&accountID, &cycleID, &userID, &keyID, &reserved, &state, &reconciliation)
		if err != nil {
			return err
		}
		if accountID != cmd.AccountID || userID != cmd.UserID || keyID != cmd.APIKeyID || cycleID != reservation.CycleID ||
			(state != "sent" && state != "uncertain" && (state != "released" || reconciliation == "")) {
			return service.ErrAccountUserQuotaReservationConflict
		}
		if state == "released" {
			// Late verified billing remains valid even after an administrator's no-charge reconciliation.
			reserved = 0
		}
	} else {
		cycle, err := synchronizeAccountUserQuotaCycle(ctx, tx, p, now)
		if err != nil {
			return err
		}
		// Legacy requests already running when preparation begins may finish.
		// Once ready, every controlled execution must carry a persisted ticket.
		if p.Enabled && cycle != nil && cycle.Ready {
			if p.EnforcedAt == nil || cmd.AccountUserExecutionStartedAt.IsZero() || !cmd.AccountUserExecutionStartedAt.Before(*p.EnforcedAt) {
				return service.NewAccountUserQuotaError(service.AccountUserQuotaUnavailable, "Controlled account execution is missing its admission ticket")
			}
		}
		if !cmd.AccountUserExecutionStartedAt.IsZero() && cycle != nil && cmd.AccountUserExecutionStartedAt.Before(cycle.StartsAt) {
			cycle, err = scanAccountUserQuotaCycle(tx.QueryRowContext(ctx, `SELECT `+accountUserQuotaCycleColumns+`
				FROM account_user_quota_cycles WHERE account_id=$1 AND starts_at <= $2 AND resets_at > $2 ORDER BY starts_at DESC LIMIT 1 FOR UPDATE`,
				cmd.AccountID, cmd.AccountUserExecutionStartedAt))
			if err != nil {
				return err
			}
		}
		if cycle != nil {
			cycleID = cycle.ID
		}
	}
	if cycleID != 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_user_quota_usage(cycle_id,user_id,actual_cost,reserved_cost)
			VALUES ($1,$2,$3,0) ON CONFLICT(cycle_id,user_id) DO UPDATE
			SET actual_cost=account_user_quota_usage.actual_cost+$3,
			reserved_cost=account_user_quota_usage.reserved_cost-$4,updated_at=$5`,
			cycleID, cmd.UserID, cmd.AccountUserActualCost, reserved, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_user_quota_cycles SET account_stats_cost=account_stats_cost+$2,
			version=version+1,updated_at=$3 WHERE id=$1 AND account_id=$4`, cycleID, cmd.AccountUserStatsCost, now, cmd.AccountID); err != nil {
			return err
		}
		result.AccountUserQuotaCycleID = &cycleID
	}
	if reservation != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE account_user_quota_reservations SET state='settled',actual_cost=$2,
			billing_request_id=$3,settled_at=$4,updated_at=$4 WHERE token=$1`,
			reservation.Token, cmd.AccountUserActualCost, cmd.RequestID, now); err != nil {
			return err
		}
	}
	return writeAccountUserQuotaBillingReceipt(ctx, tx, cmd, result.AccountUserQuotaCycleID)
}

func writeAccountUserQuotaBillingReceipt(ctx context.Context, tx *sql.Tx, cmd *service.UsageBillingCommand, cycleID *int64) error {
	var startedAt *time.Time
	if !cmd.AccountUserExecutionStartedAt.IsZero() {
		startedAt = &cmd.AccountUserExecutionStartedAt
	}
	_, err := tx.ExecContext(ctx, `UPDATE usage_billing_dedup SET account_id=$3,account_user_quota_cycle_id=$4,
		actual_cost=$5,account_stats_cost=$6,user_id=$7,execution_started_at=$8 WHERE request_id=$1 AND api_key_id=$2`,
		cmd.RequestID, cmd.APIKeyID, cmd.AccountID, cycleID, cmd.AccountUserActualCost, cmd.AccountUserStatsCost, cmd.UserID, startedAt)
	return err
}

func finishDuplicateAccountUserQuotaBilling(ctx context.Context, tx *sql.Tx, cmd *service.UsageBillingCommand, result *service.UsageBillingApplyResult) error {
	reservation := cmd.AccountUserQuotaReservation
	if _, err := lockAccountUserQuotaPolicy(ctx, tx, cmd.AccountID); err != nil {
		return err
	}
	var accountID, cycleID, userID, keyID int64
	var reserved float64
	var state string
	var billingID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT account_id,cycle_id,user_id,api_key_id,reserved_cost,state,billing_request_id
		FROM account_user_quota_reservations WHERE token=$1 FOR UPDATE`, reservation.Token).
		Scan(&accountID, &cycleID, &userID, &keyID, &reserved, &state, &billingID); err != nil {
		return err
	}
	if accountID != cmd.AccountID || userID != cmd.UserID || keyID != cmd.APIKeyID || cycleID != reservation.CycleID {
		return service.ErrAccountUserQuotaReservationConflict
	}
	result.AccountUserQuotaCycleID = &cycleID
	if state == "settled" {
		if !billingID.Valid || billingID.String != cmd.RequestID {
			return service.ErrAccountUserQuotaReservationConflict
		}
		return nil
	}
	if state != "sent" && state != "uncertain" {
		return service.ErrAccountUserQuotaReservationConflict
	}
	// No second money effect was applied. Complete only this unused reservation.
	if _, err := tx.ExecContext(ctx, `UPDATE account_user_quota_usage SET reserved_cost=reserved_cost-$3,updated_at=NOW()
		WHERE cycle_id=$1 AND user_id=$2`, cycleID, userID, reserved); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE account_user_quota_reservations SET state='settled',actual_cost=0,
		billing_request_id=$2,settled_at=NOW(),updated_at=NOW() WHERE token=$1`, reservation.Token, cmd.RequestID)
	return err
}
