package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func WriteAccountUserQuotaWSError(ctx context.Context, conn *coderws.Conn, err error) *OpenAIWSClientCloseError {
	var quotaErr *AccountUserQuotaError
	if !errors.As(err, &quotaErr) {
		return nil
	}
	status := coderws.StatusTryAgainLater
	errType := "server_error"
	if quotaErr.Status < http.StatusInternalServerError {
		status, errType = coderws.StatusPolicyViolation, "permission_error"
	}
	if quotaErr.Status == http.StatusTooManyRequests {
		errType = "rate_limit_error"
	}
	payload, marshalErr := json.Marshal(gin.H{"type": "error", "error": gin.H{
		"type": errType, "code": quotaErr.Code, "message": quotaErr.Message,
	}})
	if marshalErr == nil && conn != nil {
		writeCtx, cancel := context.WithTimeout(ctx, AccountUserQuotaOperationTimeout)
		if writeErr := conn.Write(writeCtx, coderws.MessageText, payload); writeErr != nil {
			slog.Debug("account quota websocket error write failed", "code", quotaErr.Code, "error", writeErr)
		}
		cancel()
	}
	return &OpenAIWSClientCloseError{statusCode: status, reason: quotaErr.Code, err: quotaErr}
}

// accountUserQuotaWSTurnRejection uses only replayable history already held by the engine.
func accountUserQuotaWSTurnRejection(ctx context.Context, conn *coderws.Conn, err error, payload []byte,
	history []json.RawMessage, historyExists bool, expectedPrevious, model string,
) error {
	var quotaErr *AccountUserQuotaError
	if !errors.As(err, &quotaErr) {
		return err
	}
	previous := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
	knownHistory := previous == "" || (historyExists && expectedPrevious != "" && previous == expectedPrevious)
	if len(payload) > 0 && knownHistory && quotaErr.Code != AccountUserQuotaSessionPinned {
		fullInput, fullExists, replayErr := buildOpenAIWSReplayInputSequence(history, historyExists, payload, previous != "")
		if replayErr == nil {
			retry, safe, buildErr := buildOpenAIWSCurrentTurnRetryPayload(payload, fullInput, fullExists, model)
			if safe && buildErr == nil {
				encrypted := false
				gjson.GetBytes(retry, "input").ForEach(func(_, item gjson.Result) bool {
					encrypted = encrypted || item.Get("encrypted_content").String() != ""
					return !encrypted
				})
				if !encrypted {
					return newOpenAIWSCurrentTurnFailoverError(accountUserQuotaFailover(quotaErr), retry)
				}
			}
		}
	}
	if len(payload) > 0 && quotaErr.Code == AccountUserQuotaExhausted {
		quotaErr = NewAccountUserQuotaError(AccountUserQuotaSessionPinned, "Account quota is exhausted and the conversation cannot be reconstructed on another account")
	}
	return WriteAccountUserQuotaWSError(ctx, conn, quotaErr)
}

func (s *OpenAIGatewayService) ProxyResponsesWebSocketFromClient(ctx context.Context, c *gin.Context, conn *coderws.Conn, account *Account,
	token string, first []byte, hooks *OpenAIWSIngressHooks,
) error {
	if s == nil || s.accountUserQuota == nil || account == nil || c == nil || conn == nil {
		return s.proxyResponsesWebSocketWithoutUserQuota(ctx, c, conn, account, token, first, hooks)
	}
	base := accountUserQuotaRequestFromContext(ctx)
	if base == nil {
		value, _ := c.Get("api_key")
		key, _ := value.(*APIKey)
		if key == nil {
			return NewAccountUserQuotaError(AccountUserQuotaUnavailable, "WebSocket account quota identity is unavailable")
		}
		ctx = WithAccountUserQuotaIdentity(ctx, key, nil, quotaTokenEstimate(first))
		base = accountUserQuotaRequestFromContext(ctx)
	}
	wrapped := OpenAIWSIngressHooks{}
	if hooks != nil {
		wrapped = *hooks
	}
	var mu sync.Mutex
	payloads := map[int][]byte{1: first}
	executions := make(map[int]*AccountUserQuotaExecution)
	lastModel := base.estimate.Model
	admitTurn := func(turn int) error {
		mu.Lock()
		payload := payloads[turn]
		if _, exists := executions[turn]; exists {
			mu.Unlock()
			return nil
		}
		estimate := quotaTokenEstimate(payload)
		if estimate.Model == "" {
			estimate.Model = lastModel
		} else {
			lastModel = estimate.Model
		}
		mu.Unlock()
		request := &accountUserQuotaRequest{original: base.original, subscription: base.subscription, estimate: estimate, newTurn: turn > 1}
		if selected := base.selected.Load(); selected != nil {
			copyExecution := *selected
			copyExecution.RequestedModel, copyExecution.PricingAt = estimate.Model, time.Now().UTC()
			mapping, restricted := s.ResolveChannelMappingAndRestrict(ctx, copyExecution.APIKey.GroupID, estimate.Model)
			if restricted {
				return NewAccountUserQuotaError(AccountUserQuotaUnavailable, "The current turn model is no longer allowed by the channel")
			}
			copyExecution.MappedModel, copyExecution.Mapping = mapping.MappedModel, mapping
			request.selected.Store(&copyExecution)
		}
		turnCtx := context.WithValue(ctx, accountUserQuotaRequestKey{}, request)
		execution, err := s.prepareAccountUserQuotaExecution(turnCtx, c, account, estimate)
		if err != nil {
			base.recordFailure(err)
			var quotaErr *AccountUserQuotaError
			if !errors.As(err, &quotaErr) {
				return NewAccountUserQuotaError(AccountUserQuotaUnavailable, "Current turn billing or authorization is unavailable")
			}
			return err
		}
		mu.Lock()
		executions[turn] = execution
		delete(payloads, turn)
		mu.Unlock()
		if execution != nil && (execution.Reservation != nil || accountUserQuotaIsFallback(execution)) {
			c.Set("account_user_quota_execution", execution)
		}
		return nil
	}
	// Passthrough considers its first turn admitted by the ingress handler.
	// Register the quota ticket before entering any of the transport modes.
	if err := admitTurn(1); err != nil {
		return accountUserQuotaFailover(err)
	}
	originalKey, _ := c.Get("api_key")
	if selected := base.selected.Load(); selected != nil && selected.APIKey != nil {
		c.Set("api_key", selected.APIKey)
		if selected.APIKey.Group != nil {
			ctx = context.WithValue(ctx, ctxkey.Group, selected.APIKey.Group)
		}
	}
	defer func() {
		c.Set("api_key", originalKey)
		mu.Lock()
		defer mu.Unlock()
		for _, execution := range executions {
			if execution != nil && execution.handle != nil {
				execution.handle.failed()
			}
		}
	}()
	wrapped.BeforeRequest = func(turn int, payload []byte, model string) error {
		if hooks != nil && hooks.BeforeRequest != nil {
			if err := hooks.BeforeRequest(turn, payload, model); err != nil {
				return err
			}
		}
		mu.Lock()
		payloads[turn] = payload
		if model != "" {
			lastModel = model
		}
		mu.Unlock()
		return nil
	}
	wrapped.BeforeTurn = func(turn int) error {
		if hooks != nil && hooks.BeforeTurn != nil {
			if err := hooks.BeforeTurn(turn); err != nil {
				return err
			}
		}
		return admitTurn(turn)
	}
	wrapped.AfterTurn = func(turn int, result *OpenAIForwardResult, turnErr error) {
		mu.Lock()
		execution := executions[turn]
		delete(executions, turn)
		delete(payloads, turn)
		mu.Unlock()
		if execution != nil {
			if result != nil {
				result.AccountUserQuotaExecution = execution
				if execution.handle != nil {
					execution.handle.forwardDone.Store(time.Now().UnixNano())
				}
			} else if execution.handle != nil {
				execution.handle.failed()
			}
		}
		if hooks != nil && hooks.AfterTurn != nil {
			hooks.AfterTurn(turn, result, turnErr)
		}
	}
	return s.proxyResponsesWebSocketWithoutUserQuota(ctx, c, conn, account, token, first, &wrapped)
}
