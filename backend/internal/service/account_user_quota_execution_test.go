//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type quotaLifecycleRepo struct {
	AccountUserQuotaRepository
	sent, released, uncertain int
	sendErr                   error
}

func (r *quotaLifecycleRepo) MarkSent(context.Context, string, time.Time) error {
	r.sent++
	return r.sendErr
}
func (r *quotaLifecycleRepo) ReleaseUnsent(context.Context, string, time.Time) error {
	r.released++
	return nil
}
func (r *quotaLifecycleRepo) MarkUncertain(context.Context, string, time.Time) error {
	r.uncertain++
	return nil
}

type quotaHTTPUpstream struct {
	HTTPUpstream
	client *http.Client
}

func (u *quotaHTTPUpstream) Do(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(request)
}

func TestAccountUserQuotaSendLifecycleAtActualIO(t *testing.T) {
	for _, test := range []struct {
		name                      string
		send                      bool
		markErr                   error
		sent, released, uncertain int
	}{
		{"local validation failed", false, nil, 0, 1, 0}, {"sent result unknown", true, nil, 1, 0, 1},
		{"send marker failed", true, errors.New("storage unavailable"), 1, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &quotaLifecycleRepo{sendErr: test.markErr}
			handle := newAccountUserQuotaHandle(repo, &AccountUserQuotaReservation{Token: "ticket", AccountID: 1})
			defer handle.stop()
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(200) }))
			defer upstream.Close()
			if test.send {
				svc := &OpenAIGatewayService{httpUpstream: &quotaHTTPUpstream{client: upstream.Client()}}
				ctx := context.WithValue(context.Background(), accountUserQuotaHandleKey{}, handle)
				req, err := http.NewRequestWithContext(ctx, "POST", upstream.URL, nil)
				require.NoError(t, err)
				resp, err := svc.doOpenAIUpstream(req, "", &Account{ID: 1})
				if test.markErr != nil {
					require.Error(t, err)
					require.Zero(t, calls)
				} else {
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
					require.Equal(t, 1, calls)
					require.NoError(t, handle.markSent(ctx))
				}
			}
			handle.failed()
			require.Equal(t, test.sent, repo.sent)
			require.Equal(t, test.released, repo.released)
			require.Equal(t, test.uncertain, repo.uncertain)
		})
	}
}

func TestAccountUserQuotaWebSocketUsesCurrentTurnHandle(t *testing.T) {
	first, second := &quotaLifecycleRepo{}, &quotaLifecycleRepo{}
	h1 := newAccountUserQuotaHandle(first, &AccountUserQuotaReservation{Token: "first"})
	defer h1.stop()
	h2 := newAccountUserQuotaHandle(second, &AccountUserQuotaReservation{Token: "second"})
	defer h2.stop()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("account_user_quota_execution", &AccountUserQuotaExecution{handle: h2})
	ctx := context.WithValue(context.Background(), accountUserQuotaHandleKey{}, h1)
	require.NoError(t, markAccountUserQuotaSent(ctx, c))
	require.Zero(t, first.sent)
	require.Equal(t, 1, second.sent)
}

func TestAccountUserQuotaCalibrationInputBoundaries(t *testing.T) {
	valid := InitializeAccountUserQuotaInput{ExpectedVersion: 1, CycleID: 1, HistoryVerified: true, HistoryNote: "已核对完整当前周期账单", Calibration: &AccountUserQuotaCalibrationInput{
		AuthorizePaid: true, UserID: 1, GroupID: 1, APIKeyID: 1, TotalBudget: 0.01, MaxRequests: 1, MaxOutputTokens: 32}}
	require.NoError(t, valid.Validate())
	for _, mutate := range []func(*AccountUserQuotaCalibrationInput){func(c *AccountUserQuotaCalibrationInput) { c.AuthorizePaid = false }, func(c *AccountUserQuotaCalibrationInput) { c.TotalBudget = 0 }, func(c *AccountUserQuotaCalibrationInput) { c.TotalBudget = 0.000000001 }, func(c *AccountUserQuotaCalibrationInput) { c.MaxRequests = 4 }, func(c *AccountUserQuotaCalibrationInput) { c.MaxOutputTokens = 257 }, func(c *AccountUserQuotaCalibrationInput) { c.APIKeyID = 0 }} {
		copy := valid
		calibration := *valid.Calibration
		copy.Calibration = &calibration
		mutate(copy.Calibration)
		require.Error(t, copy.Validate())
	}
}
