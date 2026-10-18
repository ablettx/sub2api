package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
)

const (
	AccountUserQuotaExhausted              = "ACCOUNT_USER_QUOTA_EXHAUSTED"
	AccountUserQuotaInitializationRequired = "ACCOUNT_USER_QUOTA_INITIALIZATION_REQUIRED"
	AccountUserQuotaEstimateUnavailable    = "ACCOUNT_USER_QUOTA_ESTIMATE_UNAVAILABLE"
	AccountUserQuotaInflight               = "ACCOUNT_USER_QUOTA_INFLIGHT"
	AccountUserQuotaSessionPinned          = "ACCOUNT_USER_QUOTA_SESSION_PINNED"
	AccountUserQuotaNotMember              = "ACCOUNT_USER_QUOTA_NOT_MEMBER"
	AccountUserQuotaUnavailable            = "ACCOUNT_USER_QUOTA_UNAVAILABLE"
	AccountUserQuotaUnmetered              = "ACCOUNT_USER_QUOTA_UNMETERED"
	AccountUserQuotaLease                  = 15 * time.Minute
	AccountUserQuotaRenewInterval          = 5 * time.Minute
	AccountUserQuotaOperationTimeout       = 2 * time.Second
	AccountUserQuotaWindow                 = 7 * 24 * time.Hour
	accountUserQuotaResetTolerance         = 2 * time.Minute
)

var ErrAccountUserQuotaConfigConflict = errors.New("account user quota configuration changed; reload before saving")
var ErrAccountUserQuotaReservationConflict = errors.New("account user quota reservation does not match the billing identity")

func NewAccountUserQuotaValidationError(message string) error {
	return infraerrors.BadRequest("ACCOUNT_USER_QUOTA_INVALID_CONFIG", message)
}

// AccountUserQuotaError is a protocol-independent admission failure.
type AccountUserQuotaError struct {
	Code     string     `json:"code"`
	Status   int        `json:"-"`
	Message  string     `json:"message"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

func (e *AccountUserQuotaError) Error() string { return e.Code + ": " + e.Message }

func NewAccountUserQuotaError(code, message string) *AccountUserQuotaError {
	status := http.StatusServiceUnavailable
	switch code {
	case AccountUserQuotaExhausted, AccountUserQuotaInflight:
		status = http.StatusTooManyRequests
	case AccountUserQuotaNotMember:
		status = http.StatusForbidden
	case AccountUserQuotaSessionPinned:
		status = http.StatusConflict
	}
	return &AccountUserQuotaError{Code: code, Status: status, Message: message}
}

type AccountUserQuotaPolicy struct {
	AccountID         int64      `json:"account_id"`
	UpstreamAccountID string     `json:"-"`
	Enabled           bool       `json:"enabled"`
	EnforcedAt        *time.Time `json:"enforced_at,omitempty"`
	Version           int64      `json:"version"`
	MemberUserIDs     []int64    `json:"member_user_ids"`
	FallbackGroupIDs  []int64    `json:"fallback_group_ids"`
	CarryoverSeconds  int        `json:"carryover_seconds"`
	HistoryNote       string     `json:"history_note"`
	HistoryVerifiedAt *time.Time `json:"history_verified_at,omitempty"`
}

type SaveAccountUserQuotaInput struct {
	ExpectedVersion  int64   `json:"expected_version"`
	Enabled          bool    `json:"enabled"`
	MemberUserIDs    []int64 `json:"member_user_ids"`
	FallbackGroupIDs []int64 `json:"fallback_group_ids"`
	CarryoverSeconds int     `json:"carryover_seconds"`
}

func (in *SaveAccountUserQuotaInput) Validate() error {
	if in == nil || in.ExpectedVersion < 0 {
		return errors.New("expected_version must be nonnegative")
	}
	if in.CarryoverSeconds < 600 || in.CarryoverSeconds > 86400 {
		return errors.New("carryover_seconds must be between 600 and 86400")
	}
	if len(in.MemberUserIDs) > 1000 || (in.Enabled && len(in.MemberUserIDs) == 0) {
		return errors.New("enabled quota requires between 1 and 1000 members")
	}
	if len(in.FallbackGroupIDs) > 16 {
		return errors.New("at most 16 fallback groups are allowed")
	}
	for name, ids := range map[string][]int64{"member_user_ids": in.MemberUserIDs, "fallback_group_ids": in.FallbackGroupIDs} {
		seen := make(map[int64]struct{}, len(ids))
		for _, id := range ids {
			if id <= 0 {
				return fmt.Errorf("%s must contain positive IDs", name)
			}
			if _, duplicate := seen[id]; duplicate {
				return fmt.Errorf("%s must not contain duplicate IDs", name)
			}
			seen[id] = struct{}{}
		}
	}
	return nil
}

type AccountUserQuotaCycle struct {
	ID                    int64      `json:"id"`
	AccountID             int64      `json:"account_id"`
	StartsAt              time.Time  `json:"starts_at"`
	ResetsAt              time.Time  `json:"resets_at"`
	Ready                 bool       `json:"ready"`
	Version               int64      `json:"version"`
	AccountStatsCost      float64    `json:"account_stats_cost"`
	Estimate              *float64   `json:"estimate"`
	EstimateAt            *time.Time `json:"estimate_at"`
	EstimateSourceCycleID *int64     `json:"estimate_source_cycle_id"`
	ObservedAt            *time.Time `json:"observed_at"`
	UsedPercent           *float64   `json:"used_percent"`
	MissingSince          *time.Time `json:"missing_since"`
	BackfillThroughID     int64      `json:"backfill_through_id"`
}

type AccountUserQuotaUsage struct {
	UserID       int64   `json:"user_id"`
	Quota        float64 `json:"quota"`
	ActualCost   float64 `json:"actual_cost"`
	ReservedCost float64 `json:"reserved_cost"`
	Remaining    float64 `json:"remaining"`
	Available    float64 `json:"available"`
}

type AccountUserQuotaSnapshot struct {
	Policy        *AccountUserQuotaPolicy `json:"policy"`
	Cycle         *AccountUserQuotaCycle  `json:"cycle"`
	EstimateState string                  `json:"estimate_state"`
	ExpiresAt     *time.Time              `json:"expires_at,omitempty"`
	Members       []AccountUserQuotaUsage `json:"members"`
	Uncertain     int                     `json:"uncertain_reservations"`
}

type AccountUserQuotaReservation struct {
	Token            string    `json:"token"`
	AccountID        int64     `json:"account_id"`
	CycleID          int64     `json:"cycle_id"`
	UserID           int64     `json:"user_id"`
	APIKeyID         int64     `json:"api_key_id"`
	OriginalGroupID  *int64    `json:"original_group_id"`
	EffectiveGroupID *int64    `json:"effective_group_id"`
	SubscriptionID   *int64    `json:"subscription_id"`
	PricingAt        time.Time `json:"pricing_at"`
	ConfigVersion    int64     `json:"config_version"`
	ReservedCost     float64   `json:"reserved_cost"`
}

type AccountUserQuotaAdmission struct {
	AccountID        int64
	UserID           int64
	APIKeyID         int64
	OriginalGroupID  *int64
	EffectiveGroupID *int64
	SubscriptionID   *int64
	RequestID        string
	PricingAt        time.Time
	ReservedCost     float64
	Unmetered        bool
	Calibration      *AccountUserQuotaCalibration
}

// AccountUserQuotaCalibration is created only by a protected, explicitly paid admin operation.
type AccountUserQuotaCalibration struct {
	RunID       string
	Ordinal     int
	MaxRequests int
	Budget      float64
	CycleID     int64
}

type AccountUserQuotaReservationDetail struct {
	AccountUserQuotaReservation
	State              string     `json:"state"`
	ActualCost         float64    `json:"actual_cost"`
	LeaseUntil         time.Time  `json:"lease_until"`
	CreatedAt          time.Time  `json:"created_at"`
	SettledAt          *time.Time `json:"settled_at,omitempty"`
	BillingRequestID   *string    `json:"billing_request_id,omitempty"`
	ReconciliationNote string     `json:"reconciliation_note"`
}

func AccountUserQuotaBudgetFits(budget, used, reserve float64) bool {
	if !finiteQuotaAmount(budget) || !finiteQuotaAmount(used) || !finiteQuotaAmount(reserve) || budget <= 0 {
		return false
	}
	return decimal.NewFromFloat(used).Add(decimal.NewFromFloat(reserve)).LessThanOrEqual(decimal.NewFromFloat(budget))
}

type AccountUserQuotaRepository interface {
	Get(context.Context, int64, time.Time) (*AccountUserQuotaSnapshot, error)
	Save(context.Context, int64, int64, *SaveAccountUserQuotaInput, time.Time) (*AccountUserQuotaSnapshot, error)
	Initialize(context.Context, int64, int64, int64, int64, string, time.Time) (*AccountUserQuotaSnapshot, error)
	Admit(context.Context, *AccountUserQuotaAdmission, time.Time) (*AccountUserQuotaReservation, error)
	MarkSent(context.Context, string, time.Time) error
	Renew(context.Context, string, time.Time) error
	ReleaseUnsent(context.Context, string, time.Time) error
	MarkUncertain(context.Context, string, time.Time) error
	GetReservation(context.Context, string) (*AccountUserQuotaReservationDetail, error)
	ListUnresolved(context.Context, int64) ([]AccountUserQuotaReservationDetail, error)
	ReconcileNoCharge(context.Context, int64, string, int64, string, time.Time) error
}

type AccountUserQuotaService struct {
	admissionMu sync.RWMutex
	draining    bool
	repo        AccountUserQuotaRepository
	cfg         *config.Config
	observer    *OpenAIQuotaService
	refreshes   sync.Map
}

func NewAccountUserQuotaService(repo AccountUserQuotaRepository, cfg *config.Config) *AccountUserQuotaService {
	return &AccountUserQuotaService{repo: repo, cfg: cfg}
}

func (s *AccountUserQuotaService) Get(ctx context.Context, accountID int64) (*AccountUserQuotaSnapshot, error) {
	now := time.Now().UTC()
	snapshot, err := s.repo.Get(ctx, accountID, now)
	if err != nil || snapshot.Policy == nil || s.observer == nil {
		return snapshot, err
	}
	cycle := snapshot.Cycle
	stale := cycle == nil || cycle.ObservedAt == nil || now.Sub(*cycle.ObservedAt) >= 10*time.Minute || !now.Before(cycle.ResetsAt) ||
		snapshot.EstimateState == "initializing" || snapshot.EstimateState == "unavailable"
	if !stale {
		return snapshot, nil
	}
	value, _ := s.refreshes.LoadOrStore(accountID, &atomic.Int64{})
	last, ok := value.(*atomic.Int64)
	if !ok {
		return nil, errors.New("invalid account quota observation refresh state")
	}
	previous := last.Load()
	boundaryCrossed := cycle != nil && !now.Before(cycle.ResetsAt) && time.Unix(0, previous).Before(cycle.ResetsAt)
	if (!boundaryCrossed && previous != 0 && now.Sub(time.Unix(0, previous)) < 10*time.Minute) || !last.CompareAndSwap(previous, now.UnixNano()) {
		return snapshot, nil
	}
	// /wham/usage reads upstream quota without creating a paid model request.
	usage, refreshErr := s.observer.QueryUsage(ctx, accountID)
	if refreshErr != nil {
		slog.Warn("account quota snapshot refresh failed", "account_id", accountID, "error", refreshErr)
		return snapshot, nil // the persisted state still controls carryover expiration
	}
	updates := buildOpenAIAutoResetUsageUpdates(usage, time.Unix(usage.FetchedAt, 0))
	if len(updates) == 0 {
		return snapshot, nil
	}
	if err := s.observer.accountRepo.UpdateExtra(ctx, accountID, updates); err != nil {
		return nil, fmt.Errorf("persist account quota observation: %w", err)
	}
	return s.repo.Get(ctx, accountID, time.Now().UTC())
}

func (s *AccountUserQuotaService) Save(ctx context.Context, accountID, actorID int64, in *SaveAccountUserQuotaInput) (*AccountUserQuotaSnapshot, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if in.Enabled && s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return nil, NewAccountUserQuotaValidationError("dynamic account quota requires standard billing mode")
	}
	return s.repo.Save(ctx, accountID, actorID, in, time.Now().UTC())
}

func (s *AccountUserQuotaService) Initialize(ctx context.Context, accountID, actorID, expectedVersion, cycleID int64, note string) (*AccountUserQuotaSnapshot, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) < 8 || utf8.RuneCountInString(note) > 1000 {
		return nil, errors.New("history verification note must contain 8 to 1000 characters")
	}
	if expectedVersion <= 0 || cycleID <= 0 || actorID <= 0 {
		return nil, errors.New("current configuration, cycle and administrator identity are required")
	}
	if _, err := s.Get(ctx, accountID); err != nil {
		return nil, err
	}
	return s.repo.Initialize(ctx, accountID, actorID, expectedVersion, cycleID, note, time.Now().UTC())
}

func (s *AccountUserQuotaService) ListUnresolved(ctx context.Context, accountID int64) ([]AccountUserQuotaReservationDetail, error) {
	return s.repo.ListUnresolved(ctx, accountID)
}

func (s *AccountUserQuotaService) ReconcileNoCharge(ctx context.Context, accountID int64, token string, actorID int64, note string) error {
	return s.repo.ReconcileNoCharge(ctx, accountID, token, actorID, note, time.Now().UTC())
}

// AccountUserQuotaEstimate preserves the existing estimate formula and billing precision.
func AccountUserQuotaEstimate(accountStats, usedPercent float64) (float64, bool) {
	if !finiteQuotaAmount(accountStats) || accountStats <= 0 || math.IsNaN(usedPercent) || math.IsInf(usedPercent, 0) || usedPercent <= 0 || usedPercent > 100 {
		return 0, false
	}
	value, _ := decimal.NewFromFloat(accountStats).Mul(decimal.NewFromInt(100)).Div(decimal.NewFromFloat(usedPercent)).Round(UsageBillingMonetaryScale).Float64()
	return value, finiteQuotaAmount(value) && value > 0
}

func finiteQuotaAmount(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value < 1e12
}

// AccountUserQuotaObservation retains source time; reading a snapshot never refreshes it.
type AccountUserQuotaObservation struct {
	ResetsAt    time.Time
	ObservedAt  time.Time
	UsedPercent *float64
}

func AccountUserQuotaObservationFromExtra(extra map[string]any, now time.Time) *AccountUserQuotaObservation {
	observed, err := parseTime(fmt.Sprint(extra["codex_usage_updated_at"]))
	if err != nil || observed.After(now.Add(time.Minute)) {
		return nil
	}
	reset, err := parseTime(fmt.Sprint(extra["codex_7d_reset_at"]))
	if err != nil {
		seconds := parseExtraInt(extra["codex_7d_reset_after_seconds"])
		if seconds <= 0 {
			return nil
		}
		reset = observed.Add(time.Duration(seconds) * time.Second)
	}
	if !reset.After(observed) || reset.Sub(observed) > AccountUserQuotaWindow+accountUserQuotaResetTolerance {
		return nil
	}
	result := &AccountUserQuotaObservation{ResetsAt: reset, ObservedAt: observed}
	if raw, ok := extra["codex_7d_used_percent"]; ok {
		percent := parseExtraFloat64(raw)
		if !math.IsNaN(percent) && !math.IsInf(percent, 0) && percent >= 0 && percent <= 100 {
			result.UsedPercent = &percent
		}
	}
	return result
}

// AccountUserQuotaSameCycle tolerates seconds of reset-header jitter.
func AccountUserQuotaSameCycle(reset, established time.Time) bool {
	return math.Abs(reset.Sub(established).Seconds()) <= accountUserQuotaResetTolerance.Seconds()
}

// AccountUserQuotaShare leaves indivisible monetary units unallocated.
func AccountUserQuotaShare(estimate float64, count int) float64 {
	if !finiteQuotaAmount(estimate) || count <= 0 {
		return 0
	}
	value, _ := decimal.NewFromFloat(estimate).Div(decimal.NewFromInt(int64(count))).Truncate(UsageBillingMonetaryScale).Float64()
	return value
}

func (s *AccountUserQuotaSnapshot) Populate(now time.Time) {
	s.EstimateState = "initializing"
	if s.Policy == nil || s.Cycle == nil {
		return
	}
	cycle := s.Cycle
	if cycle.Estimate != nil && cycle.EstimateAt != nil {
		if cycle.MissingSince == nil && cycle.EstimateSourceCycleID != nil && *cycle.EstimateSourceCycleID == cycle.ID && now.Before(cycle.ResetsAt) {
			s.EstimateState = "current"
		} else if cycle.MissingSince != nil {
			expiry := cycle.MissingSince.Add(time.Duration(s.Policy.CarryoverSeconds) * time.Second)
			s.ExpiresAt = &expiry
			if now.Before(expiry) && now.Before(cycle.ResetsAt) {
				s.EstimateState = "carryover"
			} else {
				s.EstimateState = "unavailable"
			}
		}
	}
	if !cycle.Ready {
		s.EstimateState = "initializing"
	}
	quota := 0.0
	if cycle.Estimate != nil {
		quota = AccountUserQuotaShare(*cycle.Estimate, len(s.Policy.MemberUserIDs))
	}
	byUser := make(map[int64]AccountUserQuotaUsage, len(s.Members))
	for _, member := range s.Members {
		byUser[member.UserID] = member
	}
	s.Members = make([]AccountUserQuotaUsage, 0, len(s.Policy.MemberUserIDs))
	for _, userID := range s.Policy.MemberUserIDs {
		member := byUser[userID]
		member.UserID, member.Quota = userID, quota
		member.Remaining = math.Max(QuantizeUsageBillingAmount(quota-member.ActualCost), 0)
		member.Available = math.Max(QuantizeUsageBillingAmount(member.Remaining-member.ReservedCost), 0)
		s.Members = append(s.Members, member)
	}
	sort.Slice(s.Members, func(i, j int) bool { return s.Members[i].UserID < s.Members[j].UserID })
}

func (s *AccountUserQuotaSnapshot) Check(userID int64, reserve float64, now time.Time) error {
	if s == nil || s.Policy == nil || !s.Policy.Enabled {
		return nil
	}
	if !finiteQuotaAmount(reserve) {
		return NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Cannot determine a valid request cost")
	}
	s.Populate(now)
	if s.Cycle == nil || !s.Cycle.Ready || s.EstimateState == "initializing" {
		return NewAccountUserQuotaError(AccountUserQuotaInitializationRequired, "Account quota initialization or history verification is required")
	}
	if s.EstimateState == "unavailable" || !now.Before(s.Cycle.ResetsAt) {
		return NewAccountUserQuotaError(AccountUserQuotaEstimateUnavailable, "Account quota estimate is unavailable")
	}
	for _, member := range s.Members {
		if member.UserID != userID {
			continue
		}
		if member.ActualCost >= member.Quota {
			err := NewAccountUserQuotaError(AccountUserQuotaExhausted, "Account quota has been exhausted")
			err.ResetsAt = &s.Cycle.ResetsAt
			return err
		}
		if member.Available < reserve || (reserve == 0 && member.Available <= 0) {
			return NewAccountUserQuotaError(AccountUserQuotaInflight, "Account quota is currently reserved by other requests")
		}
		return nil
	}
	return NewAccountUserQuotaError(AccountUserQuotaNotMember, "User is not a quota participant for this account")
}
