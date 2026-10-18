package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type InitializeAccountUserQuotaInput struct {
	ExpectedVersion int64                             `json:"expected_version"`
	CycleID         int64                             `json:"cycle_id"`
	HistoryVerified bool                              `json:"history_verified"`
	HistoryNote     string                            `json:"history_note"`
	Calibration     *AccountUserQuotaCalibrationInput `json:"calibration,omitempty"`
}

type AccountUserQuotaCalibrationInput struct {
	AuthorizePaid   bool    `json:"authorize_paid"`
	UserID          int64   `json:"user_id"`
	GroupID         int64   `json:"group_id"`
	APIKeyID        int64   `json:"api_key_id"`
	TotalBudget     float64 `json:"total_budget"`
	MaxRequests     int     `json:"max_requests"`
	MaxOutputTokens int     `json:"max_output_tokens"`
}

func (in *InitializeAccountUserQuotaInput) Validate() error {
	if in == nil || !in.HistoryVerified || in.ExpectedVersion <= 0 || in.CycleID <= 0 ||
		utf8.RuneCountInString(strings.TrimSpace(in.HistoryNote)) < 8 || utf8.RuneCountInString(in.HistoryNote) > 1000 {
		return errors.New("current cycle, version and verified history evidence are required")
	}
	if c := in.Calibration; c != nil {
		if !c.AuthorizePaid || c.UserID <= 0 || c.GroupID <= 0 || c.APIKeyID <= 0 ||
			!finiteQuotaAmount(c.TotalBudget) || QuantizeUsageBillingAmount(c.TotalBudget) <= 0 ||
			c.MaxRequests < 1 || c.MaxRequests > 3 || c.MaxOutputTokens < 1 || c.MaxOutputTokens > 256 {
			return errors.New("paid calibration requires an authorized user, group, key, positive budget, 1-3 requests and 1-256 output tokens")
		}
	}
	return nil
}

type AccountUserQuotaInitializationResult struct {
	Snapshot    *AccountUserQuotaSnapshot          `json:"snapshot"`
	Calibration *AccountUserQuotaCalibrationResult `json:"calibration,omitempty"`
}

type AccountUserQuotaCalibrationResult struct {
	RunID                 string  `json:"run_id"`
	Budget                float64 `json:"budget"`
	RequestCount          int     `json:"request_count"`
	ChargedCost           float64 `json:"charged_cost"`
	PendingReconciliation bool    `json:"pending_reconciliation"`
	StopReason            string  `json:"stop_reason"`
}

type accountUserQuotaCalibrationKey struct{}

func (s *OpenAIGatewayService) InitializeAccountUserQuota(ctx context.Context, caller *gin.Context, accountID, actorID int64, in *InitializeAccountUserQuotaInput) (*AccountUserQuotaInitializationResult, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if s.accountUserQuota == nil {
		return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Account quota initialization is unavailable")
	}
	snapshot, err := s.accountUserQuota.Initialize(ctx, accountID, actorID, in.ExpectedVersion, in.CycleID, in.HistoryNote)
	if err != nil {
		return nil, err
	}
	result := &AccountUserQuotaInitializationResult{Snapshot: snapshot}
	if in.Calibration == nil {
		return result, nil
	}
	calibration := in.Calibration
	if snapshot.Policy == nil || !snapshot.Policy.Enabled || !slices.Contains(snapshot.Policy.MemberUserIDs, calibration.UserID) {
		return nil, NewAccountUserQuotaValidationError("paid calibration requires an enabled policy and a participating user")
	}
	if s.accountQuotaKeys == nil || s.concurrencyService == nil {
		return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Calibration authorization or concurrency service is unavailable")
	}
	key, err := s.accountQuotaKeys.GetByID(ctx, calibration.APIKeyID)
	if err != nil || key == nil || key.UserID != calibration.UserID || derefGroupID(key.GroupID) != calibration.GroupID {
		return nil, NewAccountUserQuotaValidationError("calibration key must belong to the explicitly selected user and group")
	}
	key, subscription, err := s.RevalidateAccountUserQuotaIdentity(ctx, caller, key)
	if err != nil {
		return nil, err
	}
	if key.Group == nil || key.Group.Platform != PlatformOpenAI || !key.Group.ModelAllowlist.Allows(openai.CodexUsageProbeModel) {
		return nil, NewAccountUserQuotaValidationError("calibration group must allow the existing Codex probe model")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil || !account.IsSchedulable() || !slices.Contains(account.GroupIDs, calibration.GroupID) ||
		account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || account.IsShadow() || !account.IsModelSupported(openai.CodexUsageProbeModel) {
		return nil, NewAccountUserQuotaValidationError("calibration account is not available to the selected group and model")
	}
	budget := QuantizeUsageBillingAmount(calibration.TotalBudget)
	run := &AccountUserQuotaCalibrationResult{RunID: uuid.NewString(), Budget: budget, StopReason: "sample_limit_reached"}
	result.Calibration = run
	for ordinal := 1; ordinal <= calibration.MaxRequests; ordinal++ {
		if result.Snapshot.EstimateState == "current" || result.Snapshot.EstimateState == "carryover" {
			run.StopReason = "estimate_available"
			break
		}
		marker := &AccountUserQuotaCalibration{RunID: run.RunID, Ordinal: ordinal, MaxRequests: calibration.MaxRequests, Budget: budget, CycleID: in.CycleID}
		ticket, sampleErr := s.runAccountUserQuotaCalibrationSample(ctx, caller, account, key, subscription, marker, calibration.MaxOutputTokens)
		if ticket != nil {
			run.RequestCount++
			detail, detailErr := s.accountUserQuota.repo.GetReservation(ctx, ticket.Token)
			if detailErr != nil || detail.State != "settled" {
				run.PendingReconciliation, run.StopReason = true, "reconciliation_required"
				break
			}
			run.ChargedCost = QuantizeUsageBillingAmount(run.ChargedCost + detail.ActualCost)
		}
		if sampleErr != nil {
			slog.Warn("account quota calibration stopped", "account_id", accountID, "run_id", run.RunID, "error", sampleErr)
			run.StopReason = "sample_failed"
			break
		}
		if run.ChargedCost >= run.Budget {
			run.StopReason = "budget_exhausted"
			break
		}
		updated, err := s.accountUserQuota.Get(ctx, accountID)
		if err != nil {
			run.StopReason = "observation_unavailable"
			break
		}
		result.Snapshot = updated
		if updated.Cycle == nil || updated.Cycle.ID != in.CycleID {
			run.StopReason = "account_cycle_changed"
			break
		}
	}
	if updated, err := s.accountUserQuota.Get(ctx, accountID); err == nil {
		result.Snapshot = updated
	} else {
		slog.Warn("account quota initialization final snapshot unavailable", "account_id", accountID, "run_id", run.RunID, "error", err)
	}
	return result, nil
}

func (s *OpenAIGatewayService) runAccountUserQuotaCalibrationSample(ctx context.Context, caller *gin.Context, account *Account, key *APIKey, subscription *UserSubscription, marker *AccountUserQuotaCalibration, maxTokens int) (*AccountUserQuotaReservation, error) {
	userSlot, err := s.concurrencyService.AcquireUserSlot(ctx, key.UserID, key.User.Concurrency)
	if err != nil || userSlot == nil || !userSlot.Acquired {
		return nil, errors.New("calibration user concurrency is unavailable")
	}
	defer userSlot.ReleaseFunc()
	accountSlot, err := s.concurrencyService.AcquireAccountSlot(ctx, account.ID, account.Concurrency)
	if err != nil || accountSlot == nil || !accountSlot.Acquired {
		return nil, errors.New("calibration account concurrency is unavailable")
	}
	defer accountSlot.ReleaseFunc()
	body, err := json.Marshal(map[string]any{"model": openai.CodexUsageProbeModel, "input": "Reply exactly OK.", "max_output_tokens": maxTokens, "stream": false, "store": false})
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, accountUserQuotaCalibrationKey{}, marker)
	ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "")
	ctx = context.WithValue(ctx, ctxkey.RequestID, fmt.Sprintf("quota-init:%s:%d", marker.RunID, marker.Ordinal))
	ctx = context.WithValue(ctx, ctxkey.UserID, key.UserID)
	ctx = WithAccountUserQuotaIdentity(ctx, key, subscription, quotaTokenEstimate(body))
	accountUserQuotaRequestFromContext(ctx).newTurn = true
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request, err = http.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.Request.Header.Set("Content-Type", "application/json")
	if caller != nil {
		trusted := s.cfg != nil && s.cfg.TrustForwardedIPForAPIKeyACL()
		if clientIP := ip.GetSecurityClientIP(caller, trusted); clientIP != "" {
			c.Request.RemoteAddr = net.JoinHostPort(clientIP, "0")
		}
	}
	c.Set("api_key", key)
	c.Set("subscription", subscription)
	forwardResult, forwardErr := s.Forward(ctx, c, account, body)
	execution := AccountUserQuotaExecutionFromGin(c)
	var ticket *AccountUserQuotaReservation
	if execution != nil {
		ticket = execution.Reservation
	}
	if forwardResult != nil {
		billingErr := s.RecordUsage(ctx, &OpenAIRecordUsageInput{Result: forwardResult, APIKey: key, User: key.User,
			Account: account, Subscription: subscription, PricingAt: time.Now().UTC(), QuotaPlatform: PlatformOpenAI,
			APIKeyService: s.accountQuotaKeys, RequestPayloadHash: HashUsageRequestPayload(body)})
		if billingErr != nil {
			return ticket, billingErr
		}
	} else if mark := GetOpsCyberPolicy(c); mark != nil {
		s.RecordCyberPolicyUsageLog(ctx, CyberPolicyUsageInput{APIKey: key, Account: account, Subscription: subscription,
			AccountUserQuotaExecution: execution, Model: openai.CodexUsageProbeModel, RequestID: resolveUsageBillingRequestID(ctx, ""),
			InputTokens: mark.UpstreamInTok, OutputTokens: mark.UpstreamOutTok, APIKeyService: s.accountQuotaKeys,
			RequestPayloadHash: HashUsageRequestPayload(body)})
	}
	return ticket, forwardErr
}
