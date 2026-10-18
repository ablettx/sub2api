package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/gin-gonic/gin"
)

// AccountUserQuotaExecution is an immutable snapshot owned by one selected attempt.
// APIKey is a copy: fallback never edits the original key or its persisted binding.
type AccountUserQuotaExecution struct {
	OriginalAPIKey *APIKey
	APIKey         *APIKey
	Subscription   *UserSubscription
	RequestedModel string
	MappedModel    string
	Mapping        ChannelMappingResult
	AccountID      int64
	PricingAt      time.Time
	Reservation    *AccountUserQuotaReservation
	handle         *accountUserQuotaHandle
}

func accountUserQuotaIsFallback(execution *AccountUserQuotaExecution) bool {
	return execution != nil && derefGroupID(execution.OriginalAPIKey.GroupID) != derefGroupID(execution.APIKey.GroupID)
}

type accountUserQuotaRequestKey struct{}

type accountUserQuotaRequest struct {
	original     *APIKey
	subscription *UserSubscription
	estimate     InflightEstimateRequest
	selected     atomic.Pointer[AccountUserQuotaExecution]
	attempts     atomic.Int64
	newTurn      bool
	mu           sync.Mutex
	fallbacks    []int64
	lastFailure  error
}

func (r *accountUserQuotaRequest) rememberFallbacks(ids []int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		found := false
		for _, previous := range r.fallbacks {
			found = found || previous == id
		}
		if !found {
			r.fallbacks = append(r.fallbacks, id)
		}
	}
}

func (r *accountUserQuotaRequest) recordFailure(err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastFailure = err
}

func WithAccountUserQuotaIdentity(ctx context.Context, key *APIKey, subscription *UserSubscription, estimate InflightEstimateRequest) context.Context {
	if key == nil {
		return ctx
	}
	copyKey := *key
	return context.WithValue(ctx, accountUserQuotaRequestKey{}, &accountUserQuotaRequest{
		original: &copyKey, subscription: subscription, estimate: estimate,
	})
}

func accountUserQuotaRequestFromContext(ctx context.Context) *accountUserQuotaRequest {
	if ctx == nil {
		return nil
	}
	request, _ := ctx.Value(accountUserQuotaRequestKey{}).(*accountUserQuotaRequest)
	return request
}

func (s *OpenAIGatewayService) selectAccountWithScheduler(
	ctx context.Context, groupID *int64, previousResponseID, sessionHash, requestedModel string,
	excludedIDs map[int64]struct{}, requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability,
	requireCompact bool, platform string, previousResponseCanMove, useUpstreamTokenCost bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	request := accountUserQuotaRequestFromContext(ctx)
	// Read/count endpoints do not register a model-execution identity.
	if s.accountUserQuota == nil || (request == nil && requiredCapability != OpenAIEndpointCapabilityLive) {
		return s.selectAccountWithoutUserQuota(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs,
			requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost)
	}
	userID, _ := ctx.Value(ctxkey.UserID).(int64)
	if request != nil {
		userID = request.original.UserID
		if request.original.User != nil {
			userID = request.original.User.ID
		}
	}
	currentGroup, currentModel, currentCtx := groupID, requestedModel, ctx
	excluded := make(map[int64]struct{}, len(excludedIDs))
	for id := range excludedIDs {
		excluded[id] = struct{}{}
	}
	var pendingGroups []int64
	seenGroups := map[int64]bool{derefGroupID(groupID): true}
	var lastQuotaError error
	if request != nil {
		request.mu.Lock()
		lastQuotaError = request.lastFailure
		for _, next := range request.fallbacks {
			if !seenGroups[next] {
				seenGroups[next] = true
				pendingGroups = append(pendingGroups, next)
			}
		}
		request.mu.Unlock()
	}
	var execution *AccountUserQuotaExecution
	var decision OpenAIAccountScheduleDecision
	for attempts := 0; attempts < openAIAccountSelectionProbeLimit; attempts++ {
		if request != nil && request.attempts.Add(1) > openAIAccountSelectionProbeLimit {
			break
		}
		selection, nextDecision, err := s.selectAccountWithoutUserQuota(currentCtx, currentGroup, previousResponseID, sessionHash, currentModel,
			excluded, requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost)
		decision = nextDecision
		if err != nil || selection == nil || selection.Account == nil {
			if lastQuotaError == nil {
				return selection, decision, err
			}
			moved := false
			for len(pendingGroups) > 0 && request != nil {
				next := pendingGroups[0]
				pendingGroups = pendingGroups[1:]
				if previousResponseID != "" && !previousResponseCanMove {
					return nil, decision, NewAccountUserQuotaError(AccountUserQuotaSessionPinned, "Account quota is exhausted and this session cannot move to another account")
				}
				candidate, targetErr := s.resolveAccountUserQuotaFallback(ctx, request, next)
				if targetErr != nil {
					// An invalid target is excluded, never used as an authorization shortcut.
					slog.Debug("account quota fallback target rejected", "group_id", next, "user_id", userID, "error", targetErr)
					continue
				}
				execution = candidate
				currentGroup, currentModel = candidate.APIKey.GroupID, candidate.MappedModel
				currentCtx = context.WithValue(ctx, ctxkey.Group, candidate.APIKey.Group)
				currentCtx, _ = s.WithOpenAITurnPricingContext(currentCtx, currentGroup)
				moved = true
				break
			}
			if moved {
				continue
			}
			return nil, decision, lastQuotaError
		}
		account := selection.Account
		snapshot := &AccountUserQuotaSnapshot{}
		var quotaErr error
		if account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth {
			snapshot, quotaErr = s.accountUserQuota.Get(ctx, account.ID)
		}
		if quotaErr == nil && snapshot.Policy != nil && snapshot.Policy.Enabled {
			if request != nil {
				request.rememberFallbacks(snapshot.Policy.FallbackGroupIDs)
			}
			if requiredCapability == OpenAIEndpointCapabilityLive {
				quotaErr = NewAccountUserQuotaError(AccountUserQuotaUnmetered, "Live cannot meter a controlled account; another authorized account is required")
			} else {
				quotaErr = snapshot.Check(userID, 0, time.Now().UTC())
			}
			for _, next := range snapshot.Policy.FallbackGroupIDs {
				if !seenGroups[next] {
					seenGroups[next] = true
					pendingGroups = append(pendingGroups, next)
				}
			}
		}
		if quotaErr == nil && request != nil && request.original.Group != nil {
			group := request.original.Group
			if (group.RequirePrivacySet && !account.IsPrivacySet()) || (group.RequireOAuthOnly && account.Type == AccountTypeAPIKey) {
				quotaErr = NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Account does not satisfy the original group constraints")
			}
		}
		if quotaErr != nil {
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			excluded[account.ID] = struct{}{}
			var typed *AccountUserQuotaError
			if !errors.As(quotaErr, &typed) {
				return nil, decision, fmt.Errorf("%w: %v", NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Account quota storage is unavailable"), quotaErr)
			}
			lastQuotaError = quotaErr
			request.recordFailure(quotaErr)
			if previousResponseID != "" && !previousResponseCanMove {
				return nil, decision, NewAccountUserQuotaError(AccountUserQuotaSessionPinned, "Account quota is unavailable and this session cannot move to another account")
			}
			continue
		}
		if request != nil {
			if execution == nil {
				execution = &AccountUserQuotaExecution{OriginalAPIKey: request.original, APIKey: request.original,
					Subscription: request.subscription, RequestedModel: request.estimate.Model, MappedModel: requestedModel,
					PricingAt: OpenAIPricingAtFromContext(ctx)}
			}
			copyExecution := *execution
			copyExecution.AccountID = account.ID
			request.selected.Store(&copyExecution)
		}
		return selection, decision, nil
	}
	if lastQuotaError != nil {
		return nil, decision, lastQuotaError
	}
	return nil, decision, ErrNoAvailableAccounts
}

func (s *OpenAIGatewayService) resolveAccountUserQuotaFallback(ctx context.Context, request *accountUserQuotaRequest, groupID int64) (*AccountUserQuotaExecution, error) {
	if s.accountQuotaGroups == nil || request == nil || request.original == nil || request.original.User == nil {
		return nil, errors.New("fallback identity is unavailable")
	}
	user, err := s.userRepo.GetByID(ctx, request.original.User.ID)
	if err != nil || user == nil || !user.IsActive() {
		return nil, errors.New("fallback user is unavailable")
	}
	group, err := s.accountQuotaGroups.GetByID(ctx, groupID)
	if err != nil || group == nil || group.Status != StatusActive || group.Platform != PlatformOpenAI {
		return nil, errors.New("fallback group is unavailable")
	}
	model := request.estimate.Model
	if !group.ModelAllowlist.Allows(model) || (request.original.Group != nil && !request.original.Group.ModelAllowlist.Allows(model)) {
		return nil, errors.New("fallback model is not allowed")
	}
	var subscription *UserSubscription
	if group.IsSubscriptionType() {
		subscription, err = s.userSubRepo.GetActiveByUserIDAndGroupID(ctx, user.ID, groupID)
		if err != nil || subscription == nil || !subscription.IsActive() {
			return nil, errors.New("fallback subscription is unavailable")
		}
	} else if !user.CanBindGroup(groupID, group.IsExclusive) {
		return nil, errors.New("fallback group access is not allowed")
	}
	copyGroup := *group
	if request.original.Group != nil {
		copyGroup.RequirePrivacySet = copyGroup.RequirePrivacySet || request.original.Group.RequirePrivacySet
		copyGroup.RequireOAuthOnly = copyGroup.RequireOAuthOnly || request.original.Group.RequireOAuthOnly
	}
	key := *request.original
	key.GroupID, key.Group, key.User = &copyGroup.ID, &copyGroup, user
	if s.billingCacheService != nil {
		if err := s.billingCacheService.CheckBillingEligibilityWithoutRPM(ctx, user, &key, &copyGroup, subscription, PlatformOpenAI); err != nil {
			return nil, err
		}
	}
	mapping, restricted := s.ResolveChannelMappingAndRestrict(ctx, key.GroupID, model)
	if restricted {
		return nil, errors.New("fallback channel does not allow the model")
	}
	return &AccountUserQuotaExecution{OriginalAPIKey: request.original, APIKey: &key, Subscription: subscription,
		RequestedModel: model, MappedModel: mapping.MappedModel, Mapping: mapping, PricingAt: time.Now().UTC()}, nil
}

func AccountUserQuotaEffectiveAPIKey(ctx context.Context, original *APIKey) *APIKey {
	if request := accountUserQuotaRequestFromContext(ctx); request != nil {
		if execution := request.selected.Load(); execution != nil && execution.APIKey != nil {
			return execution.APIKey
		}
	}
	return original
}

// RevalidateAccountUserQuotaIdentity is also used when an async task begins execution.
func (s *OpenAIGatewayService) RevalidateAccountUserQuotaIdentity(ctx context.Context, c *gin.Context, original *APIKey) (*APIKey, *UserSubscription, error) {
	if original == nil || s.accountQuotaKeys == nil || s.accountQuotaGroups == nil {
		return nil, nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Current key authorization is unavailable")
	}
	key, err := s.accountQuotaKeys.GetByID(ctx, original.ID)
	if err != nil || key == nil || key.UserID != original.UserID || !key.IsActive() || key.IsExpired() || key.IsQuotaExhausted() {
		return nil, nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "API key access is no longer valid")
	}
	if derefGroupID(key.GroupID) != derefGroupID(original.GroupID) {
		return nil, nil, NewAccountUserQuotaError(AccountUserQuotaSessionPinned, "API key group changed; start a new request")
	}
	user, err := s.userRepo.GetByID(ctx, key.UserID)
	if err != nil || user == nil || !user.IsActive() {
		return nil, nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "User access is no longer valid")
	}
	if c != nil && (len(key.IPWhitelist) > 0 || len(key.IPBlacklist) > 0) {
		trustForwarded := s.cfg != nil && s.cfg.TrustForwardedIPForAPIKeyACL()
		clientIP := ip.GetSecurityClientIP(c, trustForwarded)
		if allowed, _ := ip.CheckIPRestrictionWithCompiledRules(clientIP, key.CompiledIPWhitelist, key.CompiledIPBlacklist); !allowed {
			return nil, nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "API key access is no longer valid")
		}
	}
	copyKey := *key
	copyKey.User = user
	var subscription *UserSubscription
	if key.GroupID != nil {
		group, err := s.accountQuotaGroups.GetByID(ctx, *key.GroupID)
		if err != nil || group == nil || group.Status != StatusActive {
			return nil, nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Group access is no longer valid")
		}
		copyKey.Group = group
		if group.IsSubscriptionType() {
			subscription, err = s.userSubRepo.GetActiveByUserIDAndGroupID(ctx, user.ID, group.ID)
			if err != nil || subscription == nil || !subscription.IsActive() {
				return nil, nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Subscription access is no longer valid")
			}
		} else if !user.CanBindGroup(group.ID, group.IsExclusive) {
			return nil, nil, NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Group access is no longer allowed")
		}
	}
	if s.billingCacheService != nil {
		if err := s.billingCacheService.CheckBillingEligibilityWithoutRPM(ctx, user, &copyKey, copyKey.Group, subscription, QuotaPlatform(ctx, &copyKey)); err != nil {
			return nil, nil, err
		}
	}
	return &copyKey, subscription, nil
}
