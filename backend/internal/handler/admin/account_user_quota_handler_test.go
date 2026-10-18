//go:build unit

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type quotaAdminRepo struct {
	service.AccountUserQuotaRepository
	saved int
	err   error
}

func TestAccountUserQuotaMaintenanceFailureDoesNotRetryBusinessOperation(t *testing.T) {
	original := service.DefaultIdempotencyCoordinator()
	service.SetDefaultIdempotencyCoordinator(nil)
	t.Cleanup(func() { service.SetDefaultIdempotencyCoordinator(original) })
	calls := 0
	router := gin.New()
	router.POST("/restart", func(c *gin.Context) {
		executeAdminIdempotentJSONFailOpenOnStoreUnavailable(c, "admin.system.restart", nil, time.Minute, func(context.Context) (any, error) {
			calls++
			return nil, infraerrors.ServiceUnavailable(service.AccountUserQuotaUnavailable, "Requests are still settling; restart was canceled")
		})
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/restart", nil))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), service.AccountUserQuotaUnavailable)
	require.Equal(t, 1, calls, "a business 503 must not be mistaken for an unavailable idempotency store")
	require.Empty(t, recorder.Header().Get("X-Idempotency-Degraded"))
}

func (r *quotaAdminRepo) Get(context.Context, int64, time.Time) (*service.AccountUserQuotaSnapshot, error) {
	return &service.AccountUserQuotaSnapshot{Members: []service.AccountUserQuotaUsage{}}, nil
}
func (r *quotaAdminRepo) Save(context.Context, int64, int64, *service.SaveAccountUserQuotaInput, time.Time) (*service.AccountUserQuotaSnapshot, error) {
	r.saved++
	return &service.AccountUserQuotaSnapshot{}, r.err
}

func TestAccountUserQuotaAdminStrictContractAndVersionConflict(t *testing.T) {
	for _, test := range []struct {
		name, body    string
		actor         bool
		err           error
		status, saves int
	}{
		{"valid", `{"enabled":true,"expected_version":0,"member_user_ids":[1,2,3],"fallback_group_ids":[],"carryover_seconds":1800}`, true, nil, 200, 1},
		{"identity required", `{}`, false, nil, 401, 0},
		{"unknown input", `{"clear_used":true}`, true, nil, 400, 0},
		{"multiple objects", `{} {}`, true, nil, 400, 0},
		{"duplicate members", `{"enabled":true,"member_user_ids":[1,1],"carryover_seconds":1800}`, true, nil, 400, 0},
		{"invalid duration", `{"enabled":true,"member_user_ids":[1],"carryover_seconds":0}`, true, nil, 400, 0},
		{"oversized", `{"history_note":"` + strings.Repeat("x", 65536) + `"}`, true, nil, 400, 0},
		{"version conflict", `{"enabled":true,"expected_version":2,"member_user_ids":[1],"carryover_seconds":1800}`, true, service.ErrAccountUserQuotaConfigConflict, 409, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &quotaAdminRepo{err: test.err}
			handler := &AccountHandler{accountUserQuota: service.NewAccountUserQuotaService(repo, &config.Config{})}
			router := gin.New()
			router.PUT("/accounts/:id/user-quota", func(c *gin.Context) {
				if test.actor {
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
				}
				handler.SaveUserQuota(c)
			})
			request := httptest.NewRequest(http.MethodPut, "/accounts/7/user-quota", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			require.Equal(t, test.status, recorder.Code, recorder.Body.String())
			require.Equal(t, test.saves, repo.saved)
		})
	}
}
