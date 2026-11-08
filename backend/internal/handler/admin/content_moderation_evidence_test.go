package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestContentModerationCapacityHandlerRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, body       string
		status, capacity int
	}{
		{"custom", `{"log_storage_limit_gb":25}`, 200, 25},
		{"minimum", `{"log_storage_limit_gb":1}`, 200, 1},
		{"maximum", `{"log_storage_limit_gb":200}`, 200, 200},
		{"omitted", `{}`, 200, 75},
		{"zero", `{"log_storage_limit_gb":0}`, 400, 75},
		{"negative", `{"log_storage_limit_gb":-1}`, 400, 75},
		{"above maximum", `{"log_storage_limit_gb":201}`, 400, 75},
		{"fraction", `{"log_storage_limit_gb":1.5}`, 400, 75},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := &moderationEngineSettings{value: `{"log_storage_limit_gb":75}`}
			svc := service.NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.PUT("/config", NewContentModerationHandler(svc).UpdateConfig)
			req := httptest.NewRequest(http.MethodPut, "/config", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			var stored struct {
				Capacity int `json:"log_storage_limit_gb"`
			}
			require.NoError(t, json.Unmarshal([]byte(settings.value), &stored))
			require.Equal(t, tc.capacity, stored.Capacity)
			// A new service must read the saved setting, rather than an in-memory draft.
			fresh := service.NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil)
			router.GET("/config", NewContentModerationHandler(fresh).GetConfig)
			w = httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/config", nil))
			require.Equal(t, 200, w.Code)
			var reply struct {
				Data struct {
					Capacity int `json:"log_storage_limit_gb"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &reply))
			require.Equal(t, tc.capacity, reply.Data.Capacity)
		})
	}
}

type moderationInputRepo struct {
	service.ContentModerationRepository
	calls int
}

func (r *moderationInputRepo) GetLogInput(_ context.Context, id int64) (*service.ContentModerationLogInput, error) {
	r.calls++
	if id == 404 {
		return nil, infraerrors.NotFound("CONTENT_MODERATION_LOG_NOT_FOUND", "gone")
	}
	return &service.ContentModerationLogInput{Evidence: &service.ContentModerationInputEvidence{Text: "完整审核输入"}}, nil
}

func TestContentModerationInputHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		id            string
		restricted    bool
		status, calls int
	}{
		{"1", false, 200, 1}, {"404", false, 404, 1}, {"0", false, 400, 0},
		{"abc", false, 400, 0}, {"-1", false, 400, 0}, {"1", true, 403, 0},
	} {
		repo := &moderationInputRepo{}
		svc := service.NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
		router := gin.New()
		router.GET("/logs/:id/input", NewContentModerationHandler(svc).GetLogInput)
		req := httptest.NewRequest(http.MethodGet, "/logs/"+tc.id+"/input", nil)
		if tc.restricted {
			req = req.WithContext(adminaccess.WithPrincipal(req.Context(), &adminaccess.Principal{}))
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, tc.status, w.Code)
		require.Equal(t, tc.calls, repo.calls)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		if tc.status == 200 {
			require.Contains(t, w.Body.String(), "完整审核输入")
		}
	}
}
