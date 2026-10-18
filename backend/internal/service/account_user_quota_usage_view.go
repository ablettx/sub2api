package service

import (
	"log/slog"
	"time"
)

func (s *AccountUsageService) applyOpenAIQuotaEstimate(usage *UsageInfo, quota *AccountUserQuotaSnapshot, quotaErr error, now time.Time) {
	if quotaErr != nil {
		usage.QuotaEstimateState = "unavailable"
		slog.Warn("account quota usage view unavailable", "error", quotaErr)
		return
	}
	if quota != nil && quota.Policy != nil && quota.Policy.Enabled {
		quota.Populate(now)
		usage.QuotaEstimateState, usage.QuotaEstimateExpiresAt = quota.EstimateState, quota.ExpiresAt
		if cycle := quota.Cycle; cycle != nil {
			usage.OpenAISevenDayEstimatedTotalCost, usage.QuotaEstimateAt = cycle.Estimate, cycle.EstimateAt
			if usage.SevenDay == nil {
				usage.SevenDay = &UsageProgress{}
			}
			usage.SevenDay.ResetsAt = &cycle.ResetsAt
			if cycle.UsedPercent != nil {
				usage.SevenDay.Utilization = *cycle.UsedPercent
			}
			usage.SevenDay.RemainingSeconds = max(int(cycle.ResetsAt.Sub(now).Seconds()), 0)
			if usage.SevenDay.WindowStats != nil {
				usage.SevenDay.WindowStats.Cost = cycle.AccountStatsCost
			}
		}
		return
	}
	if window := usage.SevenDay; window != nil && window.WindowStats != nil {
		if estimate, ok := AccountUserQuotaEstimate(window.WindowStats.Cost, window.Utilization); ok {
			usage.OpenAISevenDayEstimatedTotalCost = &estimate
		}
	}
}
