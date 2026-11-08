//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountRiskManualTestStoppedAccount(t *testing.T) {
	for _, scenario := range []string{"match", "mismatch", "upstream 429", "incomplete", "busy", "long reply"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, loads, _ := riskHarness()
			accounts.pool[0].Schedulable = false
			p := s.probe.(*riskTestProbe)
			p.result = AccountRiskProbeResult{Completed: true, Text: "21", InputTokens: 10, OutputTokens: 2}
			switch scenario {
			case "mismatch":
				p.result.Text = "wrong"
			case "upstream 429":
				p.err = errors.New("429")
			case "incomplete":
				p.result.Completed = false
			case "busy":
				loads.busy = true
			case "long reply":
				p.result.Text = strings.Repeat("x", 5000) + "21"
			}
			config := store.state.Rules[7].Config
			config.ProbePrompt = "unsaved probe message"
			out, err := s.TestNow(context.Background(), 7, 1, 1, config)
			require.NoError(t, err)
			require.Empty(t, store.stopped)
			require.Nil(t, store.state.Pending)
			require.False(t, accounts.pool[0].Schedulable)
			require.NotEqual(t, config.ProbePrompt, store.state.Rules[7].Config.ProbePrompt)
			if scenario == "upstream 429" || scenario == "incomplete" || scenario == "busy" {
				require.False(t, out.Completed)
				require.Nil(t, out.Matched)
			} else {
				require.True(t, out.Completed)
				require.NotNil(t, out.Matched)
				require.Equal(t, scenario != "mismatch", *out.Matched)
				require.Equal(t, config, p.lastConfig)
				require.Zero(t, loads.slots)
			}
			if scenario == "long reply" {
				require.True(t, out.Truncated)
				require.Len(t, out.Text, 4096)
				require.True(t, *out.Matched)
			}
		})
	}
}

func TestAccountRiskManualGuards(t *testing.T) {
	for _, scenario := range []string{"pending", "revision", "membership", "inactive group"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			revision := int64(1)
			switch scenario {
			case "pending":
				store.state.Pending = &AccountRiskPending{ID: "other", ExpiresAt: now.Add(time.Minute)}
			case "revision":
				revision = 0
			case "membership":
				accounts.pool[0].GroupIDs = nil
			case "inactive group":
				s.groups = &riskTestGroups{inactive: true}
			}
			_, err := s.TestNow(context.Background(), 7, 1, revision, store.state.Rules[7].Config)
			require.Error(t, err)
			require.Zero(t, s.probe.(*riskTestProbe).calls)
			if scenario == "pending" {
				require.Equal(t, "other", store.state.Pending.ID)
			}
		})
	}
}
