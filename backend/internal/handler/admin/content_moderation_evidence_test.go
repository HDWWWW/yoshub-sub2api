package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

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
