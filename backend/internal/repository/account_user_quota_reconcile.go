package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const accountUserQuotaReservationColumns = `token,account_id,cycle_id,user_id,api_key_id,original_group_id,
	effective_group_id,subscription_id,pricing_at,config_version,reserved_cost,state,actual_cost,lease_until,created_at,
	settled_at,billing_request_id,reconciliation_note`

func scanAccountUserQuotaReservation(row interface{ Scan(...any) error }) (*service.AccountUserQuotaReservationDetail, error) {
	detail := &service.AccountUserQuotaReservationDetail{}
	err := row.Scan(&detail.Token, &detail.AccountID, &detail.CycleID, &detail.UserID, &detail.APIKeyID,
		&detail.OriginalGroupID, &detail.EffectiveGroupID, &detail.SubscriptionID, &detail.PricingAt,
		&detail.ConfigVersion, &detail.ReservedCost, &detail.State, &detail.ActualCost, &detail.LeaseUntil,
		&detail.CreatedAt, &detail.SettledAt, &detail.BillingRequestID, &detail.ReconciliationNote)
	return detail, err
}

func (r *accountUserQuotaRepository) GetReservation(ctx context.Context, token string) (*service.AccountUserQuotaReservationDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, service.AccountUserQuotaOperationTimeout)
	defer cancel()
	return scanAccountUserQuotaReservation(r.db.QueryRowContext(ctx, `SELECT `+accountUserQuotaReservationColumns+`
		FROM account_user_quota_reservations WHERE token=$1`, token))
}

func (r *accountUserQuotaRepository) ListUnresolved(ctx context.Context, accountID int64) ([]service.AccountUserQuotaReservationDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, service.AccountUserQuotaOperationTimeout)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, `SELECT `+accountUserQuotaReservationColumns+` FROM account_user_quota_reservations
		WHERE account_id=$1 AND state IN ('reserved','sent','uncertain') ORDER BY created_at,id LIMIT 100`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []service.AccountUserQuotaReservationDetail{}
	for rows.Next() {
		detail, err := scanAccountUserQuotaReservation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *detail)
	}
	return result, rows.Err()
}

func (r *accountUserQuotaRepository) ReconcileNoCharge(ctx context.Context, accountID int64, token string, actorID int64, note string, now time.Time) error {
	if actorID <= 0 || utf8.RuneCountInString(note) < 8 || utf8.RuneCountInString(note) > 950 {
		return errors.New("administrator and reconciliation evidence are required")
	}
	note = fmt.Sprintf("admin:%d %s", actorID, note)
	return r.transact(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := lockAccountUserQuotaPolicy(ctx, tx, accountID); err != nil {
			return err
		}
		detail, err := scanAccountUserQuotaReservation(tx.QueryRowContext(ctx, `SELECT `+accountUserQuotaReservationColumns+`
			FROM account_user_quota_reservations WHERE token=$1 AND account_id=$2 FOR UPDATE`, token, accountID))
		if err != nil {
			return err
		}
		if detail.State == "released" && detail.ReconciliationNote == note {
			return nil
		}
		if detail.State == "settled" || detail.State == "released" ||
			(detail.State != "uncertain" && now.Before(detail.LeaseUntil)) {
			return service.ErrAccountUserQuotaReservationConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_user_quota_usage SET reserved_cost=reserved_cost-$3,updated_at=$4
			WHERE cycle_id=$1 AND user_id=$2`, detail.CycleID, detail.UserID, detail.ReservedCost, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE account_user_quota_reservations SET state='released',reconciliation_note=$2,
			settled_at=$3,updated_at=$3 WHERE token=$1`, token, note, now)
		return err
	})
}
