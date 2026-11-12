//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFenduduOrderUsesDurableRequestID(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		requests = append(requests, body)
		_, _ = w.Write([]byte(`{"success":true,"created":1,"cycle_ids":["rvc_fixture"]}`))
	}))
	defer server.Close()
	p := newHTTPFenduduProvider()
	p.base = server.URL
	job := AccountRefillJob{ID: "d8bd88cd-b4a3-5637-92b3-171cf37587c3", Plan: "pro20x", FenduduConnectionID: "s2e_fixture", DeliveryGroupIDs: []int64{2, 5}}
	_, err := p.Order(context.Background(), "rvak_fixture", job)
	require.NoError(t, err)
	// A persisted/reloaded task must reproduce the same complete payload.
	raw, err := json.Marshal(job)
	require.NoError(t, err)
	var restored AccountRefillJob
	require.NoError(t, json.Unmarshal(raw, &restored))
	_, err = p.Order(context.Background(), "rvak_fixture", restored)
	require.NoError(t, err)
	require.Equal(t, requests[0], requests[1])
	job.ID = "b475c65c-ae25-5da8-b6ed-4c3a55527927"
	_, err = p.Order(context.Background(), "rvak_fixture", job)
	require.NoError(t, err)
	require.NotEqual(t, requests[0]["request_id"], requests[2]["request_id"])
	job.ID = ""
	_, err = p.Order(context.Background(), "rvak_fixture", job)
	require.Error(t, err)
	require.Len(t, requests, 3, "invalid task IDs must never reach the paid endpoint")
}

func TestFenduduOrderReceiptStates(t *testing.T) {
	for _, tc := range []struct {
		name, body, status, supplierID, message string
		httpStatus                              int
	}{
		{"rejected", `{"success":false,"error":"请提供唯一 request_id，重试使用同一编号"}`, "failed", "", "请提供唯一 request_id", 200},
		{"pending-with-id", `{"success":true,"pending":true,"cycle_ids":["rvc_fixture"]}`, "processing", "rvc_fixture", "", 200},
		{"existing-order", `{"success":true,"created":0,"cycle_ids":["rvc_fixture"],"warning":"原订单继续处理"}`, "processing", "rvc_fixture", "原订单继续处理", 200},
		{"pending-without-id", `{"success":true,"pending":true,"created":0,"cycle_ids":[]}`, "processing", "", "", 200},
		{"missing-success", `{"error":"不完整响应"}`, "blocked", "", "不完整响应", 200},
		{"null-success", `{"success":null,"error":"不完整响应"}`, "blocked", "", "不完整响应", 200},
		{"contradictory-rejection", `{"success":false,"created":1,"cycle_ids":["rvc_fixture"],"error":"待核对"}`, "blocked", "", "待核对", 200},
		{"multiple-orders", `{"success":true,"created":2,"cycle_ids":["rvc_a","rvc_b"]}`, "blocked", "", "", 200},
		{"http400", `{"success":false,"error":"该产品暂停下单"}`, "failed", "", "该产品暂停下单", 400},
		{"http409", `{"success":false,"error":"同一 request_id 不能用于不同下单参数"}`, "blocked", "", "同一 request_id", 409},
		{"http429", `{"success":false,"error":"请求过于频繁"}`, "blocked", "", "请求过于频繁", 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, store, _, accounts := fenduduHarness(t)
			ctx := context.Background()
			require.NoError(t, s.Manual(ctx, fenduduRequest()))
			posts, details := 0, 0
			created := time.Now().UTC().Format(time.RFC3339Nano)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/order":
					posts++
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(tc.httpStatus)
					_, _ = w.Write([]byte(tc.body))
				case "/cycle-detail":
					details++
					_, _ = fmt.Fprintf(w, `{"success":true,"cycle":{"id":"rvc_fixture","product":"pro20x","status":"paying","created_at":%q}}`, created)
				case "/cycles":
					_, _ = w.Write([]byte(`{"success":true,"items":[],"has_more":false}`))
				case "/products":
					_, _ = w.Write([]byte(`{"success":true,"products":[{"product":"pro20x","available_now":5}]}`))
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
				}
			}))
			defer server.Close()
			p := newHTTPFenduduProvider()
			p.base = server.URL
			s.fendudu = p
			require.NoError(t, s.run(ctx, false))
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			job := state.Jobs[0]
			require.Equal(t, tc.status, job.Status)
			require.Equal(t, tc.supplierID, job.SupplierID)
			if tc.message != "" {
				require.Contains(t, job.SupplierError, tc.message)
			}
			require.Zero(t, accounts.adopt)
			require.Equal(t, tc.status == "failed", job.BudgetReleased)
			if tc.status == "failed" {
				require.NotNil(t, job.FinishedAt)
				require.False(t, job.ReplacementPending)
			}
			if tc.httpStatus == 429 {
				require.True(t, state.Fendudu.RetryAt.After(time.Now().Add(110*time.Second)))
			}
			// Restart/polling may only query the original order, never buy again.
			restarted := *s
			refillDue(t, store)
			require.NoError(t, restarted.run(ctx, false))
			require.Equal(t, 1, posts)
			state, err = decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Equal(t, tc.status, state.Jobs[0].Status)
			if tc.supplierID != "" {
				require.Equal(t, 1, details)
			}
		})
	}
}

func TestFenduduOrderErrorsAreRedactedAndBounded(t *testing.T) {
	for _, status := range []int{200, 400, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			message := "原因 rvak_fixture\n另一个 rvak_other_secret " + strings.Repeat("长", 1500)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": message, "warning": message})
			}))
			defer server.Close()
			p := newHTTPFenduduProvider()
			p.base = server.URL
			order, err := p.Order(context.Background(), "rvak_fixture", AccountRefillJob{ID: "d8bd88cd-b4a3-5637-92b3-171cf37587c3"})
			if status == 200 {
				require.NoError(t, err)
				require.True(t, order.rejected())
				require.Contains(t, order.Error, "原因 [REDACTED] 另一个 [REDACTED]")
				require.LessOrEqual(t, len([]rune(order.Error)), 1024)
				require.NotContains(t, order.Warning, "rvak_")
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "rvak_")
				require.Contains(t, err.Error(), "原因 [REDACTED]")
				var remote *refillProviderError
				require.ErrorAs(t, err, &remote)
				require.Equal(t, status, remote.code)
				require.Equal(t, 120*time.Second, remote.retryAfter)
			}
		})
	}
}

func TestFenduduRejectedOrderNotificationOnce(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			notifier := &refillNoticeStub{}
			s := &AccountRefillService{notifier: notifier, encryptor: refillTestEncryptor{}}
			state := accountRefillState{Config: AccountRefillConfig{Notifications: AccountRefillNotificationConfig{RefillStarted: enabled}}, EncryptedWebhook: "fixture"}
			job := AccountRefillJob{Provider: refillProviderFendudu, Plan: "pro20x", Status: "failed", SupplierStatus: "rejected", SupplierError: "该产品暂停下单"}
			persisted := false
			persist := func() error { persisted = true; return nil }
			for i := 0; i < 2; i++ {
				require.NoError(t, s.notifyRefillFailed(context.Background(), &state, &job, persist))
			}
			if enabled {
				require.True(t, persisted)
				require.Len(t, notifier.messages, 1)
				require.Contains(t, notifier.messages[0], "未创建订单")
				require.Contains(t, notifier.messages[0], job.SupplierError)
			} else {
				require.Empty(t, notifier.messages)
			}
		})
	}
}
