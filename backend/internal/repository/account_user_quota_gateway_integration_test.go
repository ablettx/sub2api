//go:build integration

package repository

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Rewrite only the transport destination. Parsing, quota admission, pricing,
// financial persistence and usage logs use the real application services.
type quotaLoopbackUpstream struct {
	service.HTTPUpstream
	target *url.URL
	client *http.Client
}

func (u *quotaLoopbackUpstream) Do(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.URL.Scheme = u.target.Scheme
	copy.URL.Host = u.target.Host
	copy.Host = u.target.Host
	return u.client.Do(copy)
}

func TestAccountUserQuotaGatewayHTTPForwardAndActualBilling(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			f := newQuotaFixture(t)
			f.ready(450, 300)
			client := testEntClient(t)
			group := mustCreateGroup(t, client, &service.Group{Name: "quota-gateway-" + uuid.NewString(), Platform: service.PlatformOpenAI, RateMultiplier: 2, AllowImageGeneration: true})
			require.NoError(t, client.Group.UpdateOneID(group.ID).SetAllowImageGeneration(true).Exec(f.ctx))
			mustBindAccountToGroup(t, client, f.account.ID, group.ID, 0)
			_, err := integrationDB.ExecContext(f.ctx, `UPDATE api_keys SET group_id=$2 WHERE id=$1`, f.keys[0].ID, group.ID)
			require.NoError(t, err)
			_, err = integrationDB.ExecContext(f.ctx, `UPDATE accounts SET credentials=credentials || '{"access_token":"isolated-test-token"}'::jsonb WHERE id=$1`, f.account.ID)
			require.NoError(t, err)
			upstreamCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls++
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Request-Id", uuid.NewString())
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_quota\",\"status\":\"completed\",\"model\":\"gpt-5.1\",\"output\":[],\"usage\":{\"input_tokens\":1000,\"output_tokens\":100,\"total_tokens\":1100}}}\n\n")
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			cfg := &config.Config{}
			cfg.Default.RateMultiplier = 1
			users := NewUserRepository(client, integrationDB)
			groups := NewGroupRepository(client, integrationDB)
			subs := NewUserSubscriptionRepository(client)
			rates := NewUserGroupRateRepository(integrationDB)
			keys := service.NewAPIKeyService(NewAPIKeyRepository(client, integrationDB), users, groups, subs, rates, nil, cfg)
			key, err := keys.GetByID(f.ctx, f.keys[0].ID)
			require.NoError(t, err)
			accounts := NewAccountRepository(client, integrationDB, nil)
			account, err := accounts.GetByID(f.ctx, f.account.ID)
			require.NoError(t, err)
			quota := service.NewAccountUserQuotaService(f.repo, cfg)
			gateway := service.ProvideOpenAIGatewayService(accounts, NewUsageLogRepository(client, integrationDB), f.billing, users, subs, rates, nil, cfg, nil, nil,
				service.NewBillingService(cfg, nil), nil, nil, &quotaLoopbackUpstream{target: target, client: server.Client()}, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, quota, groups, keys)
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"hello","max_output_tokens":128}`, stream))
			ctx := service.WithAccountUserQuotaIdentity(context.Background(), key, nil, service.InflightEstimateRequest{Model: "gpt-5.1", MaxTokens: 128, BodyBytes: len(body), Kind: service.InflightEstimateToken})
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body))).WithContext(ctx)
			c.Set("api_key", key)
			result, err := gateway.Forward(ctx, c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, result.AccountUserQuotaExecution.Reservation)
			require.Equal(t, 1, upstreamCalls)
			require.Equal(t, 1000, result.Usage.InputTokens)
			require.NoError(t, gateway.RecordUsage(ctx, &service.OpenAIRecordUsageInput{Result: result, APIKey: key, User: key.User, Account: account, APIKeyService: keys}))
			s, err := f.repo.Get(f.ctx, f.account.ID, f.now)
			require.NoError(t, err)
			require.Greater(t, s.Members[0].ActualCost, 450.0)
			require.Zero(t, s.Members[0].ReservedCost)
			var balance, logActual float64
			require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT balance FROM users WHERE id=$1`, f.users[0].ID).Scan(&balance))
			require.InDelta(t, 10000-balance, s.Members[0].ActualCost, 0.00000001)
			require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT actual_cost FROM usage_logs WHERE account_id=$1 ORDER BY id DESC LIMIT 1`, f.account.ID).Scan(&logActual))
			require.InDelta(t, logActual, s.Members[0].ActualCost-450, 0.00000001)
			// A lower estimate denies the next request before any upstream call.
			f.observe(30, f.now.Add(2*time.Second), f.reset)
			c, _ = gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body))).WithContext(ctx)
			c.Set("api_key", key)
			_, err = gateway.Forward(ctx, c, account, body)
			require.Error(t, err)
			require.Equal(t, 1, upstreamCalls)
			// Compatibility services share the same final admission boundary, including
			// callers that did not go through the Responses handler or scheduler.
			for _, endpoint := range []string{"responses/compact", "chat/completions", "messages", "alpha/search", "images/generations"} {
				t.Run(endpoint, func(t *testing.T) {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest("POST", "/v1/"+endpoint, strings.NewReader(string(body)))
					c.Set("api_key", key)
					var err error
					switch endpoint {
					case "chat/completions":
						_, err = gateway.ForwardAsChatCompletions(f.ctx, c, account, body, "", "")
					case "messages":
						_, err = gateway.ForwardAsAnthropic(f.ctx, c, account, body, "", "")
					case "alpha/search":
						_, err = gateway.ForwardAlphaSearch(f.ctx, c, account, body)
					case "images/generations":
						imageBody := []byte(`{"model":"gpt-image-1","prompt":"test","n":1}`)
						_, err = gateway.ForwardImages(f.ctx, c, account, imageBody, &service.OpenAIImagesRequest{Model: "gpt-image-1", N: 1}, "gpt-image-1")
					default:
						_, err = gateway.Forward(f.ctx, c, account, body)
					}
					var failover *service.UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Equal(t, service.GatewayFailureReason(service.AccountUserQuotaExhausted), failover.Reason)
					require.Equal(t, 1, upstreamCalls, "compatibility entry must not send an exhausted account request")
				})
			}
			// Exhaustion belongs to the user, not the shared account. Another
			// member can execute while the first member remains denied.
			_, err = integrationDB.ExecContext(f.ctx, `UPDATE api_keys SET group_id=$2 WHERE id=$1`, f.keys[1].ID, group.ID)
			require.NoError(t, err)
			otherKey, err := keys.GetByID(f.ctx, f.keys[1].ID)
			require.NoError(t, err)
			c, _ = gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
			c.Set("api_key", otherKey)
			result, err = gateway.Forward(f.ctx, c, account, body)
			require.NoError(t, err)
			require.NoError(t, gateway.RecordUsage(f.ctx, &service.OpenAIRecordUsageInput{Result: result, APIKey: otherKey, User: otherKey.User, Account: account, APIKeyService: keys}))
			require.Equal(t, 2, upstreamCalls)
			// A higher observation restores the original member without clearing
			// their historical usage or changing the account's scheduling state.
			f.observe(10, f.now.Add(3*time.Second), f.reset)
			c, _ = gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
			c.Set("api_key", key)
			result, err = gateway.Forward(f.ctx, c, account, body)
			require.NoError(t, err)
			require.NoError(t, gateway.RecordUsage(f.ctx, &service.OpenAIRecordUsageInput{Result: result, APIKey: key, User: key.User, Account: account, APIKeyService: keys}))
			require.Equal(t, 3, upstreamCalls)
			require.True(t, account.IsSchedulable())
		})
	}
}

func TestAccountUserQuotaGatewayAuthorizedSubscriptionFallback(t *testing.T) {
	f := newQuotaFixture(t)
	state := f.ready(450, 300)
	client := testEntClient(t)
	original := mustCreateGroup(t, client, &service.Group{Name: "quota-original-" + uuid.NewString(), Platform: service.PlatformOpenAI, RateMultiplier: 1})
	targetGroup := mustCreateGroup(t, client, &service.Group{Name: "quota-target-" + uuid.NewString(), Platform: service.PlatformOpenAI, RateMultiplier: 3, SubscriptionType: service.SubscriptionTypeSubscription})
	fallback := mustCreateAccount(t, client, &service.Account{Name: "quota-fallback-" + uuid.NewString(), Platform: service.PlatformOpenAI, Credentials: map[string]any{"chatgpt_account_id": uuid.NewString(), "access_token": "isolated-test-token"}, Extra: f.account.Extra})
	mustBindAccountToGroup(t, client, f.account.ID, original.ID, 0)
	mustBindAccountToGroup(t, client, fallback.ID, targetGroup.ID, 0)
	_, err := integrationDB.ExecContext(f.ctx, `UPDATE api_keys SET group_id=$2 WHERE id=$1`, f.keys[0].ID, original.ID)
	require.NoError(t, err)
	_, err = f.repo.Save(f.ctx, f.account.ID, f.users[0].ID, &service.SaveAccountUserQuotaInput{Enabled: true, ExpectedVersion: state.Policy.Version,
		MemberUserIDs: []int64{f.users[0].ID, f.users[1].ID, f.users[2].ID}, FallbackGroupIDs: []int64{targetGroup.ID}, CarryoverSeconds: 1800}, f.now)
	require.NoError(t, err)
	f.observe(25, f.now.Add(time.Second), f.reset)
	targetReceipt := f.command(0, 0, 300, nil)
	targetReceipt.AccountID = fallback.ID
	_, err = f.billing.Apply(f.ctx, targetReceipt)
	require.NoError(t, err)
	targetQuota, err := f.repo.Save(f.ctx, fallback.ID, f.users[0].ID, &service.SaveAccountUserQuotaInput{Enabled: true,
		MemberUserIDs: state.Policy.MemberUserIDs, CarryoverSeconds: 1800}, f.now)
	require.NoError(t, err)
	targetQuota, err = f.repo.Initialize(f.ctx, fallback.ID, f.users[0].ID, targetQuota.Policy.Version, targetQuota.Cycle.ID, "已核对回退账号当前周期完整账单", f.now)
	require.NoError(t, err)
	require.Zero(t, targetQuota.Members[0].ActualCost)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", uuid.NewString())
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fallback\",\"status\":\"completed\",\"model\":\"gpt-5.1\",\"output\":[],\"usage\":{\"input_tokens\":1000,\"output_tokens\":100}}}\n\n")
	}))
	defer server.Close()
	upstreamURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	users := NewUserRepository(client, integrationDB)
	groups := NewGroupRepository(client, integrationDB)
	subs := NewUserSubscriptionRepository(client)
	rates := NewUserGroupRateRepository(integrationDB)
	keys := service.NewAPIKeyService(NewAPIKeyRepository(client, integrationDB), users, groups, subs, rates, nil, cfg)
	key, err := keys.GetByID(f.ctx, f.keys[0].ID)
	require.NoError(t, err)
	rdb := testRedis(t)
	accounts := NewAccountRepository(client, integrationDB, nil)
	billingCache := service.NewBillingCacheService(NewBillingCache(rdb), users, subs, NewAPIKeyRepository(client, integrationDB), nil, rates, cfg, nil)
	t.Cleanup(billingCache.Stop)
	gateway := service.ProvideOpenAIGatewayService(accounts, NewUsageLogRepository(client, integrationDB), f.billing, users, subs, rates, NewGatewayCache(rdb), cfg, nil,
		service.NewConcurrencyService(NewConcurrencyCache(rdb, 15, 30)), service.NewBillingService(cfg, nil), nil, billingCache, &quotaLoopbackUpstream{target: upstreamURL, client: server.Client()}, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, service.NewAccountUserQuotaService(f.repo, cfg), groups, keys)
	body := []byte(`{"model":"gpt-5.1","stream":false,"input":"hello","max_output_tokens":128}`)
	newContext := func() context.Context {
		return service.WithAccountUserQuotaIdentity(context.Background(), key, nil, service.InflightEstimateRequest{Model: "gpt-5.1", MaxTokens: 128, BodyBytes: len(body), Kind: service.InflightEstimateToken})
	}
	// Configured fallback cannot supply a subscription that the user does not own.
	ctx := newContext()
	selection, _, err := gateway.SelectAccountWithScheduler(ctx, &original.ID, "", "", "gpt-5.1", nil, service.OpenAIUpstreamTransportAny, false)
	require.Error(t, err)
	require.Nil(t, selection)
	require.Zero(t, calls)
	subscription := mustCreateSubscription(t, client, &service.UserSubscription{UserID: f.users[0].ID, GroupID: targetGroup.ID})
	ctx = newContext()
	selection, _, err = gateway.SelectAccountWithScheduler(ctx, &original.ID, "", "", "gpt-5.1", nil, service.OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.Equal(t, fallback.ID, selection.Account.ID)
	if selection.ReleaseFunc != nil {
		defer selection.ReleaseFunc()
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body))).WithContext(ctx)
	c.Set("api_key", key)
	result, err := gateway.Forward(ctx, c, selection.Account, body)
	require.NoError(t, err)
	require.NoError(t, gateway.RecordUsage(ctx, &service.OpenAIRecordUsageInput{Result: result, APIKey: key, User: key.User, Account: selection.Account, APIKeyService: keys}))
	var balance, subscriptionUsed, keyUsed float64
	var keyGroup, logGroup, logOriginalGroup int64
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT balance FROM users WHERE id=$1`, f.users[0].ID).Scan(&balance))
	require.Equal(t, 9550.0, balance, "fallback subscription must not debit balance")
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT daily_usage_usd FROM user_subscriptions WHERE id=$1`, subscription.ID).Scan(&subscriptionUsed))
	require.Greater(t, subscriptionUsed, 0.0)
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT group_id,quota_used FROM api_keys WHERE id=$1`, key.ID).Scan(&keyGroup, &keyUsed))
	require.Equal(t, original.ID, keyGroup)
	require.InDelta(t, 450+subscriptionUsed, keyUsed, 0.00000001)
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT group_id,original_group_id FROM usage_logs WHERE account_id=$1 ORDER BY id DESC LIMIT 1`, fallback.ID).Scan(&logGroup, &logOriginalGroup))
	require.Equal(t, targetGroup.ID, logGroup)
	require.Equal(t, original.ID, logOriginalGroup)
	state, err = f.repo.Get(f.ctx, f.account.ID, f.now)
	require.NoError(t, err)
	require.Equal(t, 450.0, state.Members[0].ActualCost)
	targetQuota, err = f.repo.Get(f.ctx, fallback.ID, f.now)
	require.NoError(t, err)
	require.InDelta(t, subscriptionUsed, targetQuota.Members[0].ActualCost, 0.00000001, "fallback charges belong only to the target account cycle")
	require.Zero(t, targetQuota.Members[0].ReservedCost)
	ctx = newContext()
	selection, _, err = gateway.SelectAccountWithScheduler(ctx, &original.ID, "", "", "gpt-5.1", nil, service.OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	if selection.ReleaseFunc != nil {
		defer selection.ReleaseFunc()
	}
	_, err = integrationDB.ExecContext(f.ctx, `UPDATE user_subscriptions SET status='expired' WHERE id=$1`, subscription.ID)
	require.NoError(t, err)
	c, _ = gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body))).WithContext(ctx)
	c.Set("api_key", key)
	_, err = gateway.Forward(ctx, c, selection.Account, body)
	require.Error(t, err)
	require.Equal(t, 1, calls, "revoked fallback subscription must block the actual send")
	// Live has no money settlement path. Its scheduler must exclude controlled
	// accounts even when their ordinary token budget is otherwise usable.
	_, _, err = gateway.SelectAccountWithSchedulerForCapability(newContext(), &original.ID, "", "", "gpt-5.1", nil,
		service.OpenAIUpstreamTransportAny, service.OpenAIEndpointCapabilityLive, false, false, false)
	var quotaErr *service.AccountUserQuotaError
	require.ErrorAs(t, err, &quotaErr)
	require.Equal(t, service.AccountUserQuotaUnmetered, quotaErr.Code)
	uncontrolled := mustCreateAccount(t, client, &service.Account{Name: "quota-live-uncontrolled-" + uuid.NewString(), Platform: service.PlatformOpenAI,
		Credentials: map[string]any{"chatgpt_account_id": uuid.NewString()}})
	mustBindAccountToGroup(t, client, uncontrolled.ID, original.ID, 0)
	liveSelection, _, err := gateway.SelectAccountWithSchedulerForCapability(newContext(), &original.ID, "", "", "gpt-5.1", nil,
		service.OpenAIUpstreamTransportAny, service.OpenAIEndpointCapabilityLive, false, false, false)
	require.NoError(t, err)
	require.Equal(t, uncontrolled.ID, liveSelection.Account.ID)
	if liveSelection.ReleaseFunc != nil {
		liveSelection.ReleaseFunc()
	}
	require.Equal(t, 1, calls, "Live selection must not make a hidden calibration or token call")

	// Async image execution uses this same fresh identity check rather than the
	// key/user/group objects captured when the task was queued.
	for _, target := range []struct {
		table string
		id    int64
	}{{"api_keys", key.ID}, {"users", key.UserID}, {"groups", original.ID}} {
		t.Run("delayed_execution_revoked_"+target.table, func(t *testing.T) {
			// The table is a fixed test-fixture whitelist, never external input.
			_, err := integrationDB.ExecContext(f.ctx, "UPDATE "+target.table+" SET status='disabled' WHERE id=$1", target.id)
			require.NoError(t, err)
			defer func() {
				_, restoreErr := integrationDB.ExecContext(f.ctx, "UPDATE "+target.table+" SET status='active' WHERE id=$1", target.id)
				require.NoError(t, restoreErr)
			}()
			currentKey, _, err := gateway.RevalidateAccountUserQuotaIdentity(f.ctx, c, key)
			require.Error(t, err)
			require.Nil(t, currentKey)
			require.Equal(t, 1, calls)
		})
	}
}
