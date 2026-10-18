//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type quotaWSRepo struct {
	quotaLifecycleRepo
	exhausted  atomic.Bool
	checks     atomic.Int64
	admissions atomic.Int64
}

func (r *quotaWSRepo) Get(_ context.Context, _ int64, now time.Time) (*AccountUserQuotaSnapshot, error) {
	s := quotaTestSnapshot(now, 500, 1)
	s.Policy.Version = 1
	if r.exhausted.Load() {
		s.Members = []AccountUserQuotaUsage{{UserID: 1, ActualCost: 500}}
	}
	return s, nil
}
func (r *quotaWSRepo) Admit(ctx context.Context, in *AccountUserQuotaAdmission, now time.Time) (*AccountUserQuotaReservation, error) {
	r.checks.Add(1)
	s, _ := r.Get(ctx, in.AccountID, now)
	if err := s.Check(in.UserID, in.ReservedCost, now); err != nil {
		return nil, err
	}
	r.admissions.Add(1)
	return &AccountUserQuotaReservation{Token: "ws-turn-ticket", AccountID: in.AccountID, CycleID: 9, UserID: in.UserID, APIKeyID: in.APIKeyID, ReservedCost: in.ReservedCost}, nil
}

type quotaWSUsers struct {
	UserRepository
	user *User
}

func (r *quotaWSUsers) GetByID(context.Context, int64) (*User, error) { return r.user, nil }

type quotaWSGroups struct {
	GroupRepository
	group *Group
}

func (r *quotaWSGroups) GetByID(context.Context, int64) (*Group, error) { return r.group, nil }

type quotaWSKeys struct {
	APIKeyRepository
	key *APIKey
}

func (r *quotaWSKeys) GetByID(context.Context, int64) (*APIKey, error) { return r.key, nil }

type quotaWSBilling struct{ UsageBillingRepository }

func TestAccountUserQuotaWebSocketRechecksEachTurnAndProtectsOpaqueHistory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	group := &Group{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, RateMultiplier: 1}
	user := &User{ID: 1, Status: StatusActive, Balance: 1000}
	key := &APIKey{ID: 1, UserID: 1, User: user, GroupID: &group.ID, Group: group, Status: StatusActive}
	account := &Account{ID: 114, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{1},
		Credentials: map[string]any{"access_token": "isolated-test-token", "chatgpt_account_id": "00000000-0000-4000-8000-000000000001"}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
	capture := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)}}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: capture})
	defer pool.Close()
	repo := &quotaWSRepo{}
	svc := &OpenAIGatewayService{cfg: cfg, accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{*account}}, userRepo: &quotaWSUsers{user: user},
		accountQuotaGroups: &quotaWSGroups{group: group}, accountQuotaKeys: NewAPIKeyService(&quotaWSKeys{key: key}, nil, nil, nil, nil, nil, cfg),
		accountUserQuota: NewAccountUserQuotaService(repo, cfg), usageBillingRepo: &quotaWSBilling{}, billingService: NewBillingService(cfg, nil),
		httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
	finished := make(chan struct{}, 1)
	serverErr := make(chan error, 1)
	hooks := &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, err error) {
		if err == nil && result != nil {
			repo.exhausted.Store(true)
			result.AccountUserQuotaExecution.handle.stop()
			finished <- struct{}{}
		}
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, first, err := conn.Read(r.Context())
		if err != nil {
			serverErr <- err
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r
		c.Set("api_key", key)
		ctx := WithAccountUserQuotaIdentity(r.Context(), key, nil, quotaTokenEstimate(first))
		serverErr <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "isolated-test-token", first, hooks)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"hello"}],"store":false}`)))
	_, _, err = client.Read(ctx)
	require.NoError(t, err)
	select {
	case <-finished:
	case err := <-serverErr:
		t.Fatalf("first turn failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_not_retained","input":[]}`)))
	_, payload, err := client.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, AccountUserQuotaSessionPinned, gjson.GetBytes(payload, "error.code").String())
	select {
	case err := <-serverErr:
		var closed *OpenAIWSClientCloseError
		require.ErrorAs(t, err, &closed)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Equal(t, int64(2), repo.checks.Load())
	require.Equal(t, int64(1), repo.admissions.Load())
	require.Equal(t, 1, repo.sent)
	capture.mu.Lock()
	writes := len(capture.writes)
	capture.mu.Unlock()
	require.Equal(t, 1, writes, "second turn must never reach the upstream")
}

func TestAccountUserQuotaWebSocketOnlyReplaysPortableHistory(t *testing.T) {
	quotaErr := NewAccountUserQuotaError(AccountUserQuotaExhausted, "exhausted")
	history := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hello"}`)}
	err := accountUserQuotaWSTurnRejection(context.Background(), nil, quotaErr, []byte(`{"model":"gpt-5.1","previous_response_id":"resp_first","input":[{"role":"user","content":"continue"}]}`), history, true, "resp_first", "gpt-5.1")
	var retry *openAIWSCurrentTurnFailoverError
	require.ErrorAs(t, err, &retry)
	err = accountUserQuotaWSTurnRejection(context.Background(), nil, quotaErr, []byte(`{"model":"gpt-5.1","input":[{"type":"reasoning","encrypted_content":"opaque"}]}`), history, true, "resp_first", "gpt-5.1")
	var closed *OpenAIWSClientCloseError
	require.True(t, errors.As(err, &closed))
}
