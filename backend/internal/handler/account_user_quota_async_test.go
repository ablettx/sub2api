//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type quotaAsyncKeys struct {
	service.APIKeyRepository
	key      service.APIKey
	disabled atomic.Bool
}

func (r *quotaAsyncKeys) GetByID(context.Context, int64) (*service.APIKey, error) {
	key := r.key
	if r.disabled.Load() {
		key.Status = service.StatusDisabled
	}
	return &key, nil
}

func TestAccountUserQuotaAsyncImageRechecksKeyAfterQueueing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(3)
	original := &service.APIKey{ID: 9, UserID: 7, GroupID: &groupID, Status: service.StatusActive,
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, AllowImageGeneration: true}}
	repo := &quotaAsyncKeys{key: *original}
	cfg := &config.Config{}
	keys := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
	gateway := service.ProvideOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, &fakeGroupRepo{}, keys)
	store := &asyncImageMemoryStore{tasks: make(map[string]*service.ImageTaskRecord)}
	tasks := service.NewImageTaskServiceWithUploader(store, nil, time.Hour, time.Minute)
	h := &AsyncImageHandler{tasks: tasks, openAI: &OpenAIGatewayHandler{gatewayService: gateway, apiKeyService: keys}}
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	finished := make(chan struct{})
	h.execute = func(platform string, c *gin.Context) {
		defer close(finished)
		<-release
		h.executeWithGateway(platform, c)
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), original)
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: original.UserID})
		c.Next()
	})
	router.POST("/v1/images/generations/async", h.Submit)
	router.GET("/v1/images/tasks/:task_id", h.Get)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations/async", strings.NewReader(`{"model":"gpt-image-1","prompt":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	var accepted struct {
		TaskID string `json:"task_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &accepted))
	repo.disabled.Store(true)
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("queued image did not finish its authorization check")
	}
	require.Eventually(t, func() bool {
		task, err := tasks.Get(context.Background(), service.ImageTaskOwner{UserID: 7, APIKeyID: 9}, accepted.TaskID)
		return err == nil && task.Status == service.ImageTaskStatusFailed && strings.Contains(string(task.Error), "authorization or billing eligibility changed")
	}, time.Second, 10*time.Millisecond)
	for range 2 {
		poll := httptest.NewRecorder()
		router.ServeHTTP(poll, httptest.NewRequest(http.MethodGet, "/v1/images/tasks/"+accepted.TaskID, nil))
		require.Equal(t, http.StatusOK, poll.Code)
		require.Contains(t, poll.Body.String(), "permission_error")
		require.Contains(t, poll.Body.String(), service.ImageTaskStatusFailed)
	}
	require.Equal(t, service.StatusActive, original.Status, "the queued snapshot stays unchanged; execution must read current authorization")
}
