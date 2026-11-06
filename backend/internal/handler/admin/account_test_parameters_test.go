//go:build unit

package admin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type testParametersPlanRepo struct {
	service.ScheduledTestPlanRepository
	plan *service.ScheduledTestPlan
}

func (r *testParametersPlanRepo) GetByID(context.Context, int64) (*service.ScheduledTestPlan, error) {
	copy := *r.plan
	return &copy, nil
}
func (r *testParametersPlanRepo) Update(_ context.Context, p *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	r.plan = p
	return p, nil
}

func TestScheduledTestParameterPartialUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &testParametersPlanRepo{plan: &service.ScheduledTestPlan{ID: 7, ModelID: "gpt-5.4", Prompt: "keep me", ReasoningEffort: "high", CronExpression: "*/5 * * * *", Enabled: true, AutoRecover: false}}
	h := NewScheduledTestHandler(service.NewScheduledTestService(repo, nil))
	for _, body := range []string{`{"enabled":false}`, `{"prompt":"","reasoning_effort":""}`} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "7"}}
		c.Request = httptest.NewRequest("PUT", "/", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.Update(c)
		require.Equal(t, 200, w.Code)
		if strings.Contains(body, "enabled") {
			require.Equal(t, "keep me", repo.plan.Prompt)
			require.Equal(t, "high", repo.plan.ReasoningEffort)
		} else {
			require.Empty(t, repo.plan.Prompt)
			require.Empty(t, repo.plan.ReasoningEffort)
		}
		require.False(t, repo.plan.AutoRecover)
		require.False(t, repo.plan.Enabled)
	}
}

func TestManualTestRejectsInvalidParametersBeforeDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{`{"reasoning_effort":5}`, `{"reasoning_effort":"invalid"}`, `{"prompt":`, `{"prompt":"` + strings.Repeat("x", service.MaxAccountTestPromptBytes+1) + `"}`} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "7"}}
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		(&AccountHandler{}).Test(c)
		require.Equal(t, 400, w.Code)
	}
}
