package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Durable receipts take precedence over best-effort logs, including archived receipts.
// UPDATE RETURNING claims each receipt in the same transaction that adds its usage.
func backfillAccountUserQuotaReceipts(ctx context.Context, tx *sql.Tx, cycle *service.AccountUserQuotaCycle) error {
	for _, table := range []string{"usage_billing_dedup", "usage_billing_dedup_archive"} {
		query := fmt.Sprintf(`WITH captured AS (
			UPDATE %s SET account_user_quota_cycle_id=$1
			WHERE account_id=$2 AND account_user_quota_cycle_id IS NULL
			AND user_id IS NOT NULL AND actual_cost IS NOT NULL AND account_stats_cost IS NOT NULL
			AND execution_started_at >= $3 AND execution_started_at < $4
			RETURNING user_id,actual_cost,account_stats_cost
		), usage_applied AS (
			INSERT INTO account_user_quota_usage(cycle_id,user_id,actual_cost,reserved_cost)
			SELECT $1,user_id,SUM(actual_cost),0 FROM captured GROUP BY user_id
			ON CONFLICT(cycle_id,user_id) DO UPDATE
			SET actual_cost=account_user_quota_usage.actual_cost+EXCLUDED.actual_cost,updated_at=NOW()
		)
		UPDATE account_user_quota_cycles SET account_stats_cost=account_stats_cost+
		(SELECT COALESCE(SUM(account_stats_cost),0) FROM captured),version=version+1,updated_at=NOW() WHERE id=$1`, table)
		if _, err := tx.ExecContext(ctx, query, cycle.ID, cycle.AccountID, cycle.StartsAt, cycle.ResetsAt); err != nil {
			return err
		}
	}
	return nil
}
