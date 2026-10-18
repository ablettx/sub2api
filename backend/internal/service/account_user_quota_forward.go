package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type accountUserQuotaHandleKey struct{}

type accountUserQuotaHandle struct {
	repo        AccountUserQuotaRepository
	reservation *AccountUserQuotaReservation
	done        chan struct{}
	stopOnce    sync.Once
	sendOnce    sync.Once
	sendErr     error
	sent        atomic.Bool
	forwardDone atomic.Int64
}

func newAccountUserQuotaHandle(repo AccountUserQuotaRepository, reservation *AccountUserQuotaReservation) *accountUserQuotaHandle {
	handle := &accountUserQuotaHandle{repo: repo, reservation: reservation, done: make(chan struct{})}
	go handle.renew()
	return handle
}

func (h *accountUserQuotaHandle) stop() {
	if h != nil {
		h.stopOnce.Do(func() { close(h.done) })
	}
}

func (h *accountUserQuotaHandle) uncertain() {
	if h == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), AccountUserQuotaOperationTimeout)
	defer cancel()
	if err := h.repo.MarkUncertain(ctx, h.reservation.Token, time.Now().UTC()); err != nil {
		slog.Error("account quota reservation reconciliation required", "account_id", h.reservation.AccountID,
			"reservation", h.reservation.Token, "error", err)
	}
	h.stop()
}

// markSent must succeed immediately before the first billable upstream write.
// Retries of the same execution retain its original reservation.
func (h *accountUserQuotaHandle) markSent(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.sendOnce.Do(func() {
		h.sendErr = h.repo.MarkSent(ctx, h.reservation.Token, time.Now().UTC())
		if h.sendErr == nil {
			h.sent.Store(true)
		}
	})
	return h.sendErr
}

func (h *accountUserQuotaHandle) failed() {
	if h == nil {
		return
	}
	if h.sent.Load() {
		h.uncertain()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), AccountUserQuotaOperationTimeout)
	defer cancel()
	if err := h.repo.ReleaseUnsent(ctx, h.reservation.Token, time.Now().UTC()); err != nil {
		slog.Error("release unsent account quota reservation failed", "account_id", h.reservation.AccountID, "error", err)
	}
	h.stop()
}

func markAccountUserQuotaSent(ctx context.Context, c *gin.Context) error {
	handle, _ := ctx.Value(accountUserQuotaHandleKey{}).(*accountUserQuotaHandle)
	if execution := AccountUserQuotaExecutionFromGin(c); execution != nil && execution.handle != nil {
		handle = execution.handle
	}
	return handle.markSent(ctx)
}

func (h *accountUserQuotaHandle) renew() {
	ticker := time.NewTicker(AccountUserQuotaRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case now := <-ticker.C:
			// A missing billing callback retains the reservation for reconciliation.
			// It must not leave a renewal goroutine alive forever or release risk as free.
			if finished := h.forwardDone.Load(); finished != 0 && now.Sub(time.Unix(0, finished)) >= AccountUserQuotaLease {
				h.failed()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), AccountUserQuotaOperationTimeout)
			err := h.repo.Renew(ctx, h.reservation.Token, now.UTC())
			cancel()
			if err != nil {
				slog.Warn("account quota reservation renewal failed", "account_id", h.reservation.AccountID, "error", err)
				h.uncertain()
				return
			}
		}
	}
}

func AccountUserQuotaExecutionFromGin(c *gin.Context) *AccountUserQuotaExecution {
	if c == nil {
		return nil
	}
	value, _ := c.Get("account_user_quota_execution")
	execution, _ := value.(*AccountUserQuotaExecution)
	return execution
}

func accountUserQuotaFailover(err error) error {
	var quotaErr *AccountUserQuotaError
	if !errors.As(err, &quotaErr) {
		quotaErr = NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Account quota admission is unavailable")
	}
	return &UpstreamFailoverError{
		StatusCode: http.StatusServiceUnavailable, ClientStatusCode: quotaErr.Status, ClientMessage: quotaErr.Message,
		Stage: GatewayFailureStageInference, Scope: GatewayFailureScopeRequest,
		Reason: GatewayFailureReason(quotaErr.Code), RequestScopedTransient: true,
	}
}

func (s *OpenAIGatewayService) prepareAccountUserQuotaExecution(ctx context.Context, c *gin.Context, account *Account, estimate InflightEstimateRequest) (*AccountUserQuotaExecution, error) {
	if s.accountUserQuota == nil {
		return nil, nil
	}
	request := accountUserQuotaRequestFromContext(ctx)
	var execution *AccountUserQuotaExecution
	if request != nil {
		if selected := request.selected.Load(); selected != nil && selected.AccountID == account.ID {
			copyExecution := *selected
			execution = &copyExecution
		}
	}
	if execution == nil {
		value, _ := c.Get("api_key")
		key, _ := value.(*APIKey)
		if key == nil {
			return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Account quota identity is unavailable")
		}
		copyKey := *key
		subValue, _ := c.Get("subscription")
		subscription, _ := subValue.(*UserSubscription)
		execution = &AccountUserQuotaExecution{OriginalAPIKey: &copyKey, APIKey: &copyKey, Subscription: subscription,
			AccountID: account.ID, RequestedModel: estimate.Model, MappedModel: estimate.Model, PricingAt: OpenAIPricingAtFromContext(ctx)}
	}
	if execution.PricingAt.IsZero() {
		execution.PricingAt = time.Now().UTC()
	}
	controlled := false
	if account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth {
		snapshot, err := s.accountUserQuota.Get(ctx, account.ID)
		if err != nil {
			return nil, err
		}
		controlled = snapshot.Policy != nil && snapshot.Policy.Enabled
	}
	fallback := derefGroupID(execution.OriginalAPIKey.GroupID) != derefGroupID(execution.APIKey.GroupID)
	if controlled || fallback {
		key, subscription, err := s.RevalidateAccountUserQuotaIdentity(ctx, c, execution.OriginalAPIKey)
		if err != nil {
			return nil, err
		}
		localRequest := &accountUserQuotaRequest{original: key, subscription: subscription, estimate: estimate}
		if request != nil {
			localRequest.estimate = request.estimate
		}
		if key.Group != nil && !key.Group.ModelAllowlist.Allows(localRequest.estimate.Model) {
			return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "The requested model is no longer allowed")
		}
		if fallback {
			latest, err := s.resolveAccountUserQuotaFallback(ctx, localRequest, *execution.APIKey.GroupID)
			if err != nil {
				return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Fallback authorization or billing eligibility changed")
			}
			latest.AccountID, latest.PricingAt = account.ID, execution.PricingAt
			execution = latest
		} else {
			execution.OriginalAPIKey, execution.APIKey, execution.Subscription = key, key, subscription
		}
		if s.billingCacheService != nil && (fallback || (request != nil && request.newTurn)) {
			if err := s.billingCacheService.CheckBillingEligibility(ctx, execution.APIKey.User, execution.APIKey,
				execution.APIKey.Group, execution.Subscription, PlatformOpenAI); err != nil {
				return nil, err
			}
		}
		group := execution.APIKey.Group
		latestAccount, accountErr := s.accountRepo.GetByID(ctx, account.ID)
		if accountErr != nil || latestAccount == nil || !latestAccount.IsSchedulable() ||
			(group != nil && !openAIStickyAccountMatchesGroup(latestAccount, &group.ID)) {
			return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Account access or group binding is no longer available")
		}
		if group != nil && ((group.RequirePrivacySet && !latestAccount.IsPrivacySet()) || (group.RequireOAuthOnly && latestAccount.Type == AccountTypeAPIKey)) {
			return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Account does not satisfy group privacy or authentication constraints")
		}
		if localRequest.estimate.Kind == InflightEstimateImage && (!GroupAllowsImageGeneration(group) || !GroupAllowsImageGeneration(key.Group)) {
			return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Image generation is not allowed for the execution groups")
		}
	}
	if !controlled {
		if _, calibration := ctx.Value(accountUserQuotaCalibrationKey{}).(*AccountUserQuotaCalibration); calibration {
			return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Calibration was stopped because the quota policy was disabled")
		}
		return execution, nil
	}
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Controlled account quota requires standard billing mode")
	}
	if s.usageBillingRepo == nil {
		return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Atomic account quota billing is unavailable")
	}
	if request != nil {
		estimate = request.estimate
	}
	if estimate.Model == "" {
		estimate.Model = execution.RequestedModel
	}
	amount, priced := s.EstimateInflightReservation(ctx, execution.APIKey, estimate)
	if !priced {
		return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Cannot estimate this request for a controlled account")
	}
	var subscriptionID *int64
	if execution.Subscription != nil {
		subscriptionID = &execution.Subscription.ID
	}
	userID := execution.APIKey.UserID
	if execution.APIKey.User != nil {
		userID = execution.APIKey.User.ID
	}
	calibration, _ := ctx.Value(accountUserQuotaCalibrationKey{}).(*AccountUserQuotaCalibration)
	s.accountUserQuota.admissionMu.RLock()
	if s.accountUserQuota.draining {
		s.accountUserQuota.admissionMu.RUnlock()
		return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Service is draining account quota requests for a maintenance restart")
	}
	reservation, err := s.accountUserQuota.repo.Admit(ctx, &AccountUserQuotaAdmission{
		AccountID: account.ID, UserID: userID, APIKeyID: execution.APIKey.ID,
		OriginalGroupID: execution.OriginalAPIKey.GroupID, EffectiveGroupID: execution.APIKey.GroupID,
		SubscriptionID: subscriptionID, PricingAt: execution.PricingAt, ReservedCost: amount,
		Calibration: calibration,
		RequestID:   HashUsageRequestPayload([]byte(resolveUsageBillingPayloadFingerprint(ctx, ""))),
	}, time.Now().UTC())
	s.accountUserQuota.admissionMu.RUnlock()
	if err != nil || reservation == nil {
		if err == nil && calibration != nil {
			return nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Calibration admission was canceled")
		}
		return execution, err
	}
	execution.Reservation = reservation
	execution.handle = newAccountUserQuotaHandle(s.accountUserQuota.repo, reservation)
	return execution, nil
}

func (s *OpenAIGatewayService) forwardWithAccountUserQuota(ctx context.Context, c *gin.Context, account *Account, body []byte, estimate InflightEstimateRequest,
	forward func(context.Context, []byte, *AccountUserQuotaExecution) (*OpenAIForwardResult, error),
) (*OpenAIForwardResult, error) {
	if account == nil || s.accountUserQuota == nil {
		return forward(ctx, body, nil)
	}
	if handle, _ := ctx.Value(accountUserQuotaHandleKey{}).(*accountUserQuotaHandle); handle != nil && handle.reservation.AccountID == account.ID {
		return forward(ctx, body, AccountUserQuotaExecutionFromGin(c))
	}
	if execution := AccountUserQuotaExecutionFromGin(c); execution != nil && execution.AccountID == account.ID && execution.handle != nil {
		select {
		case <-execution.handle.done:
		default:
			// The WebSocket HTTP bridge already admitted this turn.
			ctx = context.WithValue(ctx, accountUserQuotaHandleKey{}, execution.handle)
			return forward(ctx, body, execution)
		}
	}
	execution, err := s.prepareAccountUserQuotaExecution(ctx, c, account, estimate)
	if err != nil {
		accountUserQuotaRequestFromContext(ctx).recordFailure(err)
		return nil, accountUserQuotaFailover(err)
	}
	if execution == nil {
		return forward(ctx, body, nil)
	}
	originalKey, _ := c.Get("api_key")
	originalSub, _ := c.Get("subscription")
	c.Set("api_key", execution.APIKey)
	c.Set("subscription", execution.Subscription)
	c.Set("account_user_quota_execution", execution)
	defer func() {
		c.Set("api_key", originalKey)
		c.Set("subscription", originalSub)
	}()
	if execution.APIKey.Group != nil {
		ctx = context.WithValue(ctx, ctxkey.Group, execution.APIKey.Group)
	}
	if execution.handle != nil {
		ctx = context.WithValue(ctx, accountUserQuotaHandleKey{}, execution.handle)
	}
	if execution.MappedModel != "" && gjson.ValidBytes(body) {
		body = ReplaceModelInBody(body, execution.MappedModel)
	}
	result, forwardErr := forward(ctx, body, execution)
	if result != nil {
		result.AccountUserQuotaExecution = execution
		if execution.handle != nil {
			execution.handle.forwardDone.Store(time.Now().UnixNano())
		}
	} else if execution.handle != nil {
		execution.handle.failed()
	}
	return result, forwardErr
}

func quotaTokenEstimate(body []byte) InflightEstimateRequest {
	maxTokens := 0
	for _, field := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if value := gjson.GetBytes(body, field); value.Int() > 0 {
			maxTokens = int(value.Int())
			break
		}
	}
	return InflightEstimateRequest{Model: gjson.GetBytes(body, "model").String(), BodyBytes: len(body), MaxTokens: maxTokens, Kind: InflightEstimateToken}
}

func (s *OpenAIGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	return s.forwardWithAccountUserQuota(ctx, c, account, body, quotaTokenEstimate(body), func(ctx context.Context, body []byte, _ *AccountUserQuotaExecution) (*OpenAIForwardResult, error) {
		return s.forwardWithoutUserQuota(ctx, c, account, body)
	})
}

func (s *OpenAIGatewayService) ForwardAsChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte, promptCacheKey, defaultMappedModel string) (*OpenAIForwardResult, error) {
	return s.forwardWithAccountUserQuota(ctx, c, account, body, quotaTokenEstimate(body), func(ctx context.Context, body []byte, execution *AccountUserQuotaExecution) (*OpenAIForwardResult, error) {
		if execution != nil && execution.MappedModel != "" {
			defaultMappedModel = execution.MappedModel
		}
		return s.forwardAsChatCompletionsWithoutUserQuota(ctx, c, account, body, promptCacheKey, defaultMappedModel)
	})
}

func (s *OpenAIGatewayService) ForwardAsAnthropic(ctx context.Context, c *gin.Context, account *Account, body []byte, promptCacheKey, defaultMappedModel string) (*OpenAIForwardResult, error) {
	return s.forwardWithAccountUserQuota(ctx, c, account, body, quotaTokenEstimate(body), func(ctx context.Context, body []byte, execution *AccountUserQuotaExecution) (*OpenAIForwardResult, error) {
		if execution != nil && execution.MappedModel != "" {
			defaultMappedModel = execution.MappedModel
		}
		return s.forwardAsAnthropicWithoutUserQuota(ctx, c, account, body, promptCacheKey, defaultMappedModel)
	})
}

func (s *OpenAIGatewayService) ForwardAlphaSearch(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	estimate := quotaTokenEstimate(body)
	estimate.SearchCalls = 1
	return s.forwardWithAccountUserQuota(ctx, c, account, body, estimate, func(ctx context.Context, body []byte, _ *AccountUserQuotaExecution) (*OpenAIForwardResult, error) {
		return s.forwardAlphaSearchWithoutUserQuota(ctx, c, account, body)
	})
}

func (s *OpenAIGatewayService) ForwardImages(ctx context.Context, c *gin.Context, account *Account, body []byte, parsed *OpenAIImagesRequest, mappedModel string) (*OpenAIForwardResult, error) {
	if parsed == nil {
		return nil, errors.New("parsed images request is required")
	}
	estimate := InflightEstimateRequest{Model: parsed.Model, BodyBytes: len(body), Kind: InflightEstimateImage, Units: parsed.N}
	return s.forwardWithAccountUserQuota(ctx, c, account, body, estimate, func(ctx context.Context, body []byte, execution *AccountUserQuotaExecution) (*OpenAIForwardResult, error) {
		if execution != nil && execution.MappedModel != "" {
			mappedModel = execution.MappedModel
		}
		return s.forwardImagesWithoutUserQuota(ctx, c, account, body, parsed, mappedModel)
	})
}
