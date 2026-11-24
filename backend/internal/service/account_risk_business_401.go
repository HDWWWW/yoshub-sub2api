package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const accountRiskBusiness401 = "business_401"

// Attribute the rejection to the request's group even if the same account serves
// several groups. A later gateway cooldown, retry or credential refresh does not
// invalidate the fact that the upstream rejected this particular request.
func (s *OpenAIGatewayService) observeBusiness401(ctx context.Context, account *Account, status int, source string) {
	if status != http.StatusUnauthorized || s == nil || s.accountRisk == nil || account == nil || account.Platform != PlatformOpenAI {
		return
	}
	var groupID int64
	if group, _ := ctx.Value(ctxkey.Group).(*Group); group != nil {
		groupID = group.ID
	}
	if groupID == 0 {
		if scope := headerOverrideScopeFromContext(ctx); scope != nil {
			groupID = scope.groupID
		}
	}
	s.recordBusiness401(ctx, groupID, account, source)
}

func (s *OpenAIGatewayService) recordBusiness401(ctx context.Context, groupID int64, account *Account, source string) {
	if groupID <= 0 || account == nil || account.Platform != PlatformOpenAI || s == nil || s.accountRisk == nil || s.accountRisk.store == nil {
		return
	}
	risk := s.accountRisk
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	for {
		queued := false
		err := risk.store.Update(persist, func(state *AccountRiskState, _ AccountRiskScheduling) error {
			rule := state.Rules[groupID]
			if rule == nil || !rule.Config.Notify401Enabled {
				return nil
			}
			appendRisk401Event(rule, account.ID, account.Name, time.Now().UTC(), accountRiskBusiness401, "gateway", "业务请求收到上游 401（"+source+"）")
			queued = true
			return nil
		})
		if err == nil {
			if queued {
				risk.wakeNotifications()
			}
			return
		}
		if !errors.Is(err, ErrAccountRiskBusy) || persist.Err() != nil {
			slog.Warn("account_risk_401_alert_failed", "group_id", groupID, "account_id", account.ID, "error", err)
			return
		}
		select {
		case <-persist.Done():
			slog.Warn("account_risk_401_alert_timeout", "group_id", groupID, "account_id", account.ID)
			return
		case <-time.After(40 * time.Millisecond):
		}
	}
}

// Some transports deliver a typed error after a successful HTTP upgrade. Two
// frames (error + response.failed) may describe the same rejected turn. The
// observer is per attempt/turn, and accepts only an explicit upstream status.
func (s *OpenAIGatewayService) accountRiskStream401(ctx context.Context, c *gin.Context, account *Account) func() {
	if s == nil || s.accountRisk == nil || account == nil || account.Platform != PlatformOpenAI {
		return nil
	}
	groupID := getOpenAIGroupIDFromContext(c)
	return sync.OnceFunc(func() {
		if groupID > 0 {
			s.recordBusiness401(ctx, groupID, account, "流式错误事件")
			return
		}
		s.observeBusiness401(ctx, account, http.StatusUnauthorized, "流式错误事件")
	})
}

func explicitStreamStatus(payload []byte) int {
	for _, path := range openAIStreamErrorStatusPaths {
		v := gjson.GetBytes(payload, path)
		if v.Type != gjson.Number && v.Type != gjson.String {
			continue
		}
		if n, err := strconv.Atoi(v.String()); err == nil && n >= 400 && n <= 599 {
			return n
		}
	}
	return 0
}

func explicitStream401(payload []byte) bool {
	if status := explicitStreamStatus(payload); status != 0 {
		return status == http.StatusUnauthorized
	}
	// A typed authentication rejection is the stream equivalent of HTTP 401.
	return openAIStreamCredentialAuthFailure(payload)
}
