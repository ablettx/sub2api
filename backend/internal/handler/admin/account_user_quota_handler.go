package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (h *AccountHandler) accountQuotaID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return 0, false
	}
	if h.accountUserQuota == nil {
		response.ErrorWithDetails(c, http.StatusServiceUnavailable, "Account quota service is unavailable", service.AccountUserQuotaUnavailable, nil)
		return 0, false
	}
	if h.adminService != nil {
		if _, err := h.adminService.GetAccount(c.Request.Context(), id); err != nil {
			response.ErrorFrom(c, err)
			return 0, false
		}
	}
	return id, true
}

func accountQuotaAdminActor(c *gin.Context) (int64, bool) {
	actor, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || actor.UserID <= 0 {
		response.Unauthorized(c, "Administrator identity is required")
		return 0, false
	}
	return actor.UserID, true
}

func bindAccountQuotaJSON(c *gin.Context, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		response.BadRequest(c, "Invalid account quota request")
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		response.BadRequest(c, "Account quota request must contain one JSON object")
		return false
	}
	return true
}

func accountQuotaAdminError(c *gin.Context, err error) {
	var quotaErr *service.AccountUserQuotaError
	if errors.As(err, &quotaErr) {
		response.ErrorWithDetails(c, quotaErr.Status, quotaErr.Message, quotaErr.Code, nil)
		return
	}
	if errors.Is(err, service.ErrAccountUserQuotaConfigConflict) || errors.Is(err, service.ErrAccountUserQuotaReservationConflict) {
		response.ErrorWithDetails(c, http.StatusConflict, "Account quota data changed; reload before retrying", "ACCOUNT_USER_QUOTA_CONFLICT", nil)
		return
	}
	response.ErrorFrom(c, err)
}

func (h *AccountHandler) GetUserQuota(c *gin.Context) {
	id, ok := h.accountQuotaID(c)
	if !ok {
		return
	}
	snapshot, err := h.accountUserQuota.Get(c.Request.Context(), id)
	if err != nil {
		accountQuotaAdminError(c, err)
		return
	}
	response.Success(c, snapshot)
}

func (h *AccountHandler) SaveUserQuota(c *gin.Context) {
	id, ok := h.accountQuotaID(c)
	if !ok {
		return
	}
	actor, ok := accountQuotaAdminActor(c)
	if !ok {
		return
	}
	var input service.SaveAccountUserQuotaInput
	if !bindAccountQuotaJSON(c, &input) {
		return
	}
	if err := input.Validate(); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	snapshot, err := h.accountUserQuota.Save(c.Request.Context(), id, actor, &input)
	if err != nil {
		accountQuotaAdminError(c, err)
		return
	}
	response.Success(c, snapshot)
}

func (h *AccountHandler) InitializeUserQuota(c *gin.Context) {
	id, ok := h.accountQuotaID(c)
	if !ok {
		return
	}
	actor, ok := accountQuotaAdminActor(c)
	if !ok {
		return
	}
	var input service.InitializeAccountUserQuotaInput
	if !bindAccountQuotaJSON(c, &input) {
		return
	}
	if err := input.Validate(); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if strings.TrimSpace(c.GetHeader("Idempotency-Key")) == "" || h.quotaGateway == nil ||
		(input.Calibration != nil && service.DefaultIdempotencyCoordinator() == nil) {
		response.BadRequest(c, "An idempotency key and available initialization service are required")
		return
	}
	// The coordinator may finish after the HTTP caller disconnects; retain only independent caller IP metadata.
	caller := c.Copy()
	trusted := h.cfg != nil && h.cfg.TrustForwardedIPForAPIKeyACL()
	caller.Request = &http.Request{RemoteAddr: net.JoinHostPort(ip.GetSecurityClientIP(c, trusted), "0"), Header: make(http.Header)}
	caller.Keys = nil
	result, err := executeAdminIdempotentWithTimeout(c, fmt.Sprintf("admin.accounts.user-quota.initialize.%d", id), input,
		service.DefaultWriteIdempotencyTTL(), 2*time.Minute, func(ctx context.Context) (any, error) {
			return h.quotaGateway.InitializeAccountUserQuota(ctx, caller, id, actor, &input)
		})
	if err != nil {
		accountQuotaAdminError(c, err)
		return
	}
	if result.Replayed {
		c.Header("X-Idempotency-Replayed", "true")
	}
	response.Success(c, result.Data)
}

func (h *AccountHandler) GetUserQuotaReservations(c *gin.Context) {
	id, ok := h.accountQuotaID(c)
	if !ok {
		return
	}
	items, err := h.accountUserQuota.ListUnresolved(c.Request.Context(), id)
	if err != nil {
		accountQuotaAdminError(c, err)
		return
	}
	response.Success(c, items)
}

func (h *AccountHandler) ReconcileUserQuotaReservation(c *gin.Context) {
	id, ok := h.accountQuotaID(c)
	if !ok {
		return
	}
	actor, ok := accountQuotaAdminActor(c)
	if !ok {
		return
	}
	token := c.Param("token")
	if _, err := uuid.Parse(token); err != nil {
		response.BadRequest(c, "Invalid reservation token")
		return
	}
	var input struct {
		ConfirmedNoCharge bool   `json:"confirmed_no_charge"`
		Evidence          string `json:"evidence"`
		Note              string `json:"note"`
	}
	if !bindAccountQuotaJSON(c, &input) {
		return
	}
	input.Evidence, input.Note = strings.TrimSpace(input.Evidence), strings.TrimSpace(input.Note)
	if !input.ConfirmedNoCharge || utf8.RuneCountInString(input.Evidence) < 8 || utf8.RuneCountInString(input.Note) < 8 ||
		utf8.RuneCountInString(input.Evidence) > 450 || utf8.RuneCountInString(input.Note) > 450 {
		response.BadRequest(c, "Confirmed no-charge evidence and a reconciliation note are required")
		return
	}
	if err := h.accountUserQuota.ReconcileNoCharge(c.Request.Context(), id, token, actor, input.Evidence+" | "+input.Note); err != nil {
		accountQuotaAdminError(c, err)
		return
	}
	response.Success(c, gin.H{"reconciled": true})
}
