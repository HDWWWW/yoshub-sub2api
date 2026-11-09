//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func refillCalendarFixture(t *testing.T) *refillWorkdayCalendar {
	t.Helper()
	c := &refillWorkdayCalendar{Years: map[int]refillWorkdayYear{}}
	for _, year := range []int{2026, 2027} {
		raw, err := os.ReadFile(fmt.Sprintf("testdata/refill-calendar/%d.json", year))
		require.NoError(t, err)
		c.Years[year], err = decodeRefillWorkdayYear(raw, year)
		require.NoError(t, err)
	}
	return c
}

func refillCalendarTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.ParseInLocation("2006-01-02 15:04", value, refillTimezone)
	require.NoError(t, err)
	return at
}

func TestRefillCalendarOfficialHolidaysMakeupDaysAndBeijingTimezone(t *testing.T) {
	c := refillCalendarFixture(t)
	for _, test := range []struct {
		date string
		work bool
	}{
		{"2026-01-02 08:00", false}, // Friday, New Year holiday.
		{"2026-01-04 08:00", true},  // Sunday, official make-up workday.
		{"2026-11-07 08:00", false}, // Ordinary Saturday.
		{"2026-11-09 08:00", true},
		{"2027-01-31 08:00", true}, // Sunday before Spring Festival.
		{"2027-02-05 08:00", false},
		{"2027-02-13 08:00", false},
		{"2027-02-14 08:00", true}, // Sunday after Spring Festival.
	} {
		t.Run(test.date, func(t *testing.T) {
			work, known := c.isWorkday(refillCalendarTime(t, test.date))
			require.True(t, known)
			require.Equal(t, test.work, work)
		})
	}
	utc, err := time.Parse(time.RFC3339, "2026-01-03T16:30:00Z")
	require.NoError(t, err)
	work, known := c.isWorkday(utc)
	require.True(t, known)
	require.True(t, work, "UTC Saturday is already the make-up Sunday in Beijing")
	_, known = c.isWorkday(refillCalendarTime(t, "2028-01-03 08:00"))
	require.False(t, known, "missing years never fall back to Monday-Friday")
	c.Years[2027].Workdays["2026-12-31"] = false
	work, known = c.isWorkday(refillCalendarTime(t, "2026-12-31 08:00"))
	require.True(t, known)
	require.False(t, work, "next year's announcement takes precedence for December")
}

func TestRefillCalendarNextRunSkipsHolidaysAndKeepsReceipts(t *testing.T) {
	c := refillCalendarFixture(t)
	schedule := &AccountRefillDailySchedule{Enabled: true, DayMode: refillChinaWorkday, Time: "08:00", Quantity: 1}
	for _, test := range []struct{ now, last, next string }{
		{"2026-01-02 07:00", "", "2026-01-04 08:00"},
		{"2026-01-04 07:59", "", "2026-01-04 08:00"},
		{"2026-01-04 08:01", "", "2026-01-05 08:00"},
		{"2026-01-04 07:59", "2026-01-04 07:00", "2026-01-05 08:00"},
		{"2026-12-31 09:00", "", "2027-01-04 08:00"},
		{"2027-02-05 07:00", "", "2027-02-14 08:00"},
	} {
		var last time.Time
		if test.last != "" {
			last = refillCalendarTime(t, test.last)
		}
		next := nextRefillDailyTime(schedule, refillCalendarTime(t, test.now), last, c)
		require.Equal(t, test.next, next.In(refillTimezone).Format("2006-01-02 15:04"))
	}
	require.True(t, nextRefillDailyTime(schedule, refillCalendarTime(t, "2027-12-31 09:00"), time.Time{}, c).IsZero())
	schedule.DayMode = ""
	require.Equal(t, "2026-01-03 08:00", nextRefillDailyTime(schedule, refillCalendarTime(t, "2026-01-02 09:00"), time.Time{}).In(refillTimezone).Format("2006-01-02 15:04"))
}

func TestRefillWorkdayPurchasesOnlyTodayAndOnce(t *testing.T) {
	for _, test := range []struct {
		now, next string
		purchase  bool
	}{
		{"2026-01-02 09:00", "2026-01-04 08:00", false},
		{"2026-01-03 09:00", "2026-01-04 08:00", false},
		{"2026-01-04 07:00", "2026-01-04 08:00", false},
		{"2026-01-04 09:00", "2026-01-05 08:00", true},
		{"2027-01-31 09:00", "2027-02-01 08:00", true},
		{"2028-01-03 09:00", "", false},
	} {
		t.Run(test.now, func(t *testing.T) {
			s, store, provider, _ := failureRefillHarness(t)
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			now := refillCalendarTime(t, test.now)
			state.WorkdayCalendar = refillCalendarFixture(t)
			state.Config.DailySchedule = &AccountRefillDailySchedule{Enabled: true, DayMode: refillChinaWorkday, Time: "08:00", Quantity: 2}
			state.Config.Enabled, state.Config.MonitorEnabled = false, false
			state.DailyScheduleStatus.NextAt = now.AddDate(0, 0, -5)
			persist := func() error {
				return saveRefillState(&state, func(raw string) error { store.raw = raw; return nil })
			}
			reserved, err := s.runRefillDailySchedule(context.Background(), &state, now, persist)
			require.NoError(t, err)
			require.Equal(t, test.purchase, reserved, state.DailyScheduleStatus.Message)
			if test.next == "" {
				require.True(t, state.DailyScheduleStatus.NextAt.IsZero())
			} else {
				require.Equal(t, test.next, state.DailyScheduleStatus.NextAt.In(refillTimezone).Format("2006-01-02 15:04"))
			}
			if test.purchase {
				require.Len(t, state.Jobs, 2)
				require.Equal(t, now.Format("2006-01-02"), state.DailyScheduleStatus.LastAt.In(refillTimezone).Format("2006-01-02"))
			} else {
				require.Empty(t, state.Jobs)
				require.Empty(t, state.Usage)
			}
			// Reload all persistent state as a restarted worker would.
			state, err = decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			reserved, err = s.runRefillDailySchedule(context.Background(), &state, now, persist)
			require.NoError(t, err)
			require.False(t, reserved)
			require.Empty(t, provider.orders, "reservation tests never make supplier purchases")
		})
	}
}

func TestRefillCalendarSyncPersistsAndRetainsCacheOnFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, _, _ := failureRefillHarness(t)
		seedDailyRefill(t, store, time.Now().Add(time.Hour), 1)
		store.mutate(t, func(state *accountRefillState) { state.Config.DailySchedule.DayMode = refillChinaWorkday })
		calls := []int{}
		s.calendarFetch = func(ctx context.Context, year int) (refillWorkdayYear, error) {
			calls = append(calls, year)
			return refillWorkdayYear{Year: year, Workdays: map[string]bool{fmt.Sprintf("%d-01-03", year): false}, Papers: []string{"https://www.gov.cn/example"}}, nil
		}
		require.NoError(t, s.syncRefillCalendar(context.Background()))
		require.Equal(t, []int{2000, 2001}, calls)
		state, err := decodeAccountRefillState(store.raw)
		require.NoError(t, err)
		require.Equal(t, []int{2000, 2001}, state.WorkdayCalendar.view().Years)
		require.Equal(t, "2000-01-04 09:00", state.DailyScheduleStatus.NextAt.In(refillTimezone).Format("2006-01-02 15:04"))
		revision, synced := state.Revision, state.WorkdayCalendar.SyncedAt
		require.NoError(t, s.syncRefillCalendar(context.Background()))
		require.Len(t, calls, 2, "calendar source is checked once per 24 hours")
		time.Sleep(24 * time.Hour)
		s.calendarFetch = func(context.Context, int) (refillWorkdayYear, error) {
			return refillWorkdayYear{}, errors.New("offline")
		}
		require.NoError(t, s.syncRefillCalendar(context.Background()))
		state, err = decodeAccountRefillState(store.raw)
		require.NoError(t, err)
		require.Len(t, state.WorkdayCalendar.Years, 2)
		require.True(t, state.WorkdayCalendar.SyncedAt.Equal(synced))
		require.True(t, state.WorkdayCalendar.NextSyncAt.Equal(time.Now().Add(time.Hour)))
		require.Contains(t, state.WorkdayCalendar.Message, "保留已缓存数据")
		require.Equal(t, revision, state.Revision)
		require.Empty(t, state.Jobs)
	})
}

func TestRefillWorkdaySkipsOnlySelectedTask(t *testing.T) {
	s, store, _, _ := failureRefillHarness(t)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	now := refillCalendarTime(t, "2026-01-02 09:00")
	state.WorkdayCalendar = refillCalendarFixture(t)
	state.Config.DailySchedules = []AccountRefillDailySchedule{
		{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Enabled: true, DayMode: refillChinaWorkday, Time: "08:00", Quantity: 2, Plan: "pro20x"},
		{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Enabled: true, Time: "08:00", Quantity: 1, Plan: "pro5x"},
	}
	state.DailyScheduleStatuses = map[string]AccountRefillDailyScheduleStatus{}
	for _, schedule := range state.Config.DailySchedules {
		state.DailyScheduleStatuses[schedule.ID] = AccountRefillDailyScheduleStatus{NextAt: now.Add(-time.Hour)}
	}
	reserved, err := s.runRefillDailySchedule(context.Background(), &state, now, func() error { return nil })
	require.NoError(t, err)
	require.True(t, reserved)
	require.Len(t, state.Jobs, 1)
	require.Equal(t, "pro5x", state.Jobs[0].Plan)
	require.True(t, state.DailyScheduleStatuses[state.Config.DailySchedules[0].ID].LastAt.IsZero())
	require.Equal(t, 1, state.DailyScheduleStatuses[state.Config.DailySchedules[1].ID].LastQuantity)
}

func TestRefillCalendarRefreshPreservesDueBatchesAndConfiguration(t *testing.T) {
	state := accountRefillState{WorkdayCalendar: refillCalendarFixture(t)}
	now := refillCalendarTime(t, "2026-01-04 09:00")
	state.Config.DailySchedules = []AccountRefillDailySchedule{{ID: "legacy", Enabled: true, DayMode: refillChinaWorkday, Time: "08:00", Quantity: 1}}
	state.DailyScheduleStatuses = map[string]AccountRefillDailyScheduleStatus{"legacy": {NextAt: now.Add(-time.Hour)}}
	refreshRefillWorkdayDeadlines(&state, now)
	require.True(t, state.DailyScheduleStatuses["legacy"].NextAt.Equal(now.Add(-time.Hour)))
	state.DailyScheduleStatuses["legacy"] = AccountRefillDailyScheduleStatus{NextAt: now.Add(-time.Hour), LastAt: now.Add(-time.Hour)}
	refreshRefillWorkdayDeadlines(&state, now)
	require.Equal(t, "2026-01-05 08:00", state.DailyScheduleStatuses["legacy"].NextAt.In(refillTimezone).Format("2006-01-02 15:04"))
	c := state.Config
	c.DailySchedules = slices.Clone(c.DailySchedules)
	c.DailySchedules[0].DayMode, c.DailySchedules[0].Time = "", "10:00"
	configureRefillDailySchedule(&state, c, now)
	require.Equal(t, "2026-01-05 10:00", state.DailyScheduleStatuses["legacy"].NextAt.In(refillTimezone).Format("2006-01-02 15:04"), "changing mode never replays today's receipt")
	c.DailySchedules[0].DayMode = "weekdays"
	require.Error(t, validateRefillDailySchedules(c))
}

func TestRefillCalendarUnpublishedYearAndConcurrentSave(t *testing.T) {
	s, store, _, _ := failureRefillHarness(t)
	now := time.Now()
	year := now.In(refillTimezone).Year()
	seedDailyRefill(t, store, now.Add(time.Hour), 1)
	store.mutate(t, func(state *accountRefillState) { state.Config.DailySchedule.DayMode = refillChinaWorkday })
	s.calendarFetch = func(ctx context.Context, target int) (refillWorkdayYear, error) {
		if target == year+1 {
			return refillWorkdayYear{}, errRefillCalendarUnpublished
		}
		// A concurrent settings save must survive the network phase.
		state, err := decodeAccountRefillState(store.raw)
		require.NoError(t, err)
		config := state.Config
		schedule := *config.DailySchedule
		schedule.Quantity = 4
		config.DailySchedule = &schedule
		require.NoError(t, s.Configure(ctx, config, "", state.Revision))
		return refillWorkdayYear{Year: year, Workdays: map[string]bool{}}, nil
	}
	require.NoError(t, s.syncRefillCalendar(context.Background()))
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, 4, state.Config.DailySchedule.Quantity)
	require.Equal(t, []int{year}, state.WorkdayCalendar.view().Years)
	require.Contains(t, state.WorkdayCalendar.Message, "次年安排尚未发布")
	require.WithinDuration(t, now.Add(24*time.Hour), state.WorkdayCalendar.NextSyncAt, time.Second)
	// Once the next year becomes available, it is added without changing settings.
	store.mutate(t, func(state *accountRefillState) { state.WorkdayCalendar.NextSyncAt = time.Time{} })
	s.calendarFetch = func(ctx context.Context, target int) (refillWorkdayYear, error) {
		return refillWorkdayYear{Year: target, Workdays: map[string]bool{}}, nil
	}
	require.NoError(t, s.syncRefillCalendar(context.Background()))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, []int{year, year + 1}, state.WorkdayCalendar.view().Years)
	require.Equal(t, 4, state.Config.DailySchedule.Quantity)
}

func TestRefillCalendarRejectsMalformedAndUnpublishedData(t *testing.T) {
	valid := `{"year":2026,"papers":["https://www.gov.cn/announcement"],"days":[{"date":"2026-01-01","isOffDay":true}]}`
	for _, raw := range []string{
		`{`, strings.Replace(valid, `"year":2026`, `"year":2027`, 1),
		strings.Replace(valid, `"isOffDay":true`, `"isOffDay":null`, 1),
		strings.Replace(valid, `"2026-01-01"`, `"2026-02-30"`, 1),
		strings.Replace(valid, `www.gov.cn`, `www.gov.cn.evil.test`, 1),
		strings.Replace(valid, `[{"date":"2026-01-01","isOffDay":true}]`, `[]`, 1),
	} {
		_, err := decodeRefillWorkdayYear([]byte(raw), 2026)
		require.Error(t, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/valid":
			_, _ = w.Write([]byte(valid))
		case "/large":
			_, _ = w.Write([]byte(strings.Repeat(" ", (128<<10)+1)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	data, err := fetchRefillWorkdayURL(context.Background(), server.Client(), server.URL+"/valid", 2026)
	require.NoError(t, err)
	require.False(t, data.Workdays["2026-01-01"])
	_, err = fetchRefillWorkdayURL(context.Background(), server.Client(), server.URL+"/large", 2026)
	require.Error(t, err)
	_, err = fetchRefillWorkdayURL(context.Background(), server.Client(), server.URL+"/missing", 2026)
	require.ErrorIs(t, err, errRefillCalendarUnpublished)
	raw, err := json.Marshal(refillCalendarFixture(t))
	require.NoError(t, err)
	var restored refillWorkdayCalendar
	require.NoError(t, json.Unmarshal(raw, &restored))
	work, known := restored.isWorkday(refillCalendarTime(t, "2027-02-14 08:00"))
	require.True(t, known)
	require.True(t, work)
}
