package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const refillChinaWorkday = "china_workday"

var errRefillCalendarUnpublished = errors.New("年度日历尚未发布")

// Keep the last verified calendars in the existing durable rule store.
// Refreshes never hold the purchase lock while waiting on a remote source.
type refillWorkdayCalendar struct {
	Years      map[int]refillWorkdayYear `json:"years"`
	CheckedAt  time.Time                 `json:"checked_at"`
	SyncedAt   time.Time                 `json:"synced_at"`
	NextSyncAt time.Time                 `json:"next_sync_at"`
	Message    string                    `json:"message"`
}

type refillWorkdayYear struct {
	Year      int             `json:"year"`
	Workdays  map[string]bool `json:"workdays"`
	Papers    []string        `json:"papers"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type AccountRefillCalendarView struct {
	Years     []int     `json:"years"`
	SyncedAt  time.Time `json:"synced_at"`
	CheckedAt time.Time `json:"checked_at"`
	Message   string    `json:"message"`
}

func (c *refillWorkdayCalendar) view() *AccountRefillCalendarView {
	if c == nil {
		return nil
	}
	view := &AccountRefillCalendarView{Years: []int{}, SyncedAt: c.SyncedAt, CheckedAt: c.CheckedAt, Message: c.Message}
	for year := range c.Years {
		view.Years = append(view.Years, year)
	}
	slices.Sort(view.Years)
	return view
}

func (c *refillWorkdayCalendar) isWorkday(at time.Time) (bool, bool) {
	if c == nil {
		return false, false
	}
	day := at.In(refillTimezone)
	year, ok := c.Years[day.Year()]
	if !ok {
		return false, false
	}
	date := day.Format("2006-01-02")
	// The following year's announcement can also adjust this December.
	if next, ok := c.Years[day.Year()+1]; ok {
		if workday, exists := next.Workdays[date]; exists {
			return workday, true
		}
	}
	if workday, exists := year.Workdays[date]; exists {
		return workday, true
	}
	return day.Weekday() != time.Saturday && day.Weekday() != time.Sunday, true
}

func refillHasWorkdaySchedule(c AccountRefillConfig) bool {
	if c.DailySchedules != nil {
		return slices.ContainsFunc(c.DailySchedules, func(row AccountRefillDailySchedule) bool {
			return row.Enabled && row.DayMode == refillChinaWorkday
		})
	}
	return c.DailySchedule != nil && c.DailySchedule.Enabled && c.DailySchedule.DayMode == refillChinaWorkday
}

func refillDailyWaitingMessage(schedule *AccountRefillDailySchedule, next time.Time) string {
	if schedule.DayMode == refillChinaWorkday {
		if next.IsZero() {
			return "等待同步中国法定工作日日历，暂不执行此任务"
		}
		return "等待中国法定工作日定时补号（含调休补班）"
	}
	return "等待每日定时补号"
}

func decodeRefillWorkdayYear(raw []byte, year int) (refillWorkdayYear, error) {
	var document struct {
		Year   int      `json:"year"`
		Papers []string `json:"papers"`
		Days   []struct {
			Date     string `json:"date"`
			IsOffDay *bool  `json:"isOffDay"`
		} `json:"days"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return refillWorkdayYear{}, errors.New("日历数据格式无效")
	}
	if document.Year != year || len(document.Papers) == 0 || len(document.Days) == 0 || len(document.Days) > 400 {
		return refillWorkdayYear{}, errors.New("日历年份不符或未包含正式放假安排")
	}
	for _, paper := range document.Papers {
		u, err := url.Parse(paper)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || (u.Hostname() != "gov.cn" && !strings.HasSuffix(u.Hostname(), ".gov.cn")) {
			return refillWorkdayYear{}, errors.New("日历缺少有效的政府公告来源")
		}
	}
	result := refillWorkdayYear{Year: year, Papers: document.Papers, Workdays: map[string]bool{}}
	hasHoliday := false
	for _, entry := range document.Days {
		date, err := time.Parse("2006-01-02", entry.Date)
		_, duplicate := result.Workdays[entry.Date]
		validYear := date.Year() == year || (date.Year() == year-1 && date.Month() == time.December)
		if err != nil || date.Format("2006-01-02") != entry.Date || !validYear || entry.IsOffDay == nil || duplicate {
			return refillWorkdayYear{}, errors.New("日历日期或调休标识无效")
		}
		result.Workdays[entry.Date] = !*entry.IsOffDay
		hasHoliday = hasHoliday || (date.Year() == year && *entry.IsOffDay)
	}
	if !hasHoliday {
		return refillWorkdayYear{}, errors.New("日历未包含本年度放假安排")
	}
	return result, nil
}

func fetchRefillWorkdayYear(ctx context.Context, year int) (refillWorkdayYear, error) {
	client := &http.Client{Timeout: 8 * time.Second}
	var lastErr error
	for _, base := range []string{
		"https://raw.githubusercontent.com/NateScarlet/holiday-cn/master/",
		"https://cdn.jsdelivr.net/gh/NateScarlet/holiday-cn@master/",
	} {
		data, err := fetchRefillWorkdayURL(ctx, client, fmt.Sprintf("%s%d.json", base, year), year)
		if err == nil {
			return data, nil
		}
		if errors.Is(err, errRefillCalendarUnpublished) || ctx.Err() != nil {
			return refillWorkdayYear{}, err
		}
		lastErr = err
	}
	return refillWorkdayYear{}, lastErr
}

func fetchRefillWorkdayURL(ctx context.Context, client *http.Client, source string, year int) (refillWorkdayYear, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return refillWorkdayYear{}, err
	}
	req.Header.Set("Accept", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return refillWorkdayYear{}, errors.New("日历源暂不可用")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return refillWorkdayYear{}, errRefillCalendarUnpublished
	}
	if response.StatusCode != http.StatusOK {
		return refillWorkdayYear{}, fmt.Errorf("日历源返回 HTTP %d", response.StatusCode)
	}
	const maxSize = 128 << 10
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSize+1))
	if err != nil || len(raw) > maxSize {
		return refillWorkdayYear{}, errors.New("日历数据读取失败或过大")
	}
	return decodeRefillWorkdayYear(raw, year)
}

func (s *AccountRefillService) wakeRefillCalendar() {
	select {
	case s.rootService().calendarWake <- struct{}{}:
	default:
	}
}

func (s *AccountRefillService) runRefillCalendarSync(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		services, err := s.allRuleServices(ctx)
		if err == nil {
			// All groups reuse each year's response during this refresh pass.
			type result struct {
				data refillWorkdayYear
				err  error
			}
			results := map[int]result{}
			fetch := s.calendarFetch
			if fetch == nil {
				fetch = fetchRefillWorkdayYear
			}
			cachedFetch := func(ctx context.Context, year int) (refillWorkdayYear, error) {
				if cached, ok := results[year]; ok {
					return cached.data, cached.err
				}
				data, err := fetch(ctx, year)
				results[year] = result{data, err}
				return data, err
			}
			for _, rule := range services {
				if ctx.Err() != nil {
					return
				}
				// Failures are persisted per rule, independently of the purchase worker.
				_ = rule.syncRefillCalendar(ctx, cachedFetch)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.calendarWake:
		}
	}
}

func (s *AccountRefillService) syncRefillCalendar(ctx context.Context, fetchers ...func(context.Context, int) (refillWorkdayYear, error)) error {
	raw, err := s.store.Read(ctx)
	if err != nil {
		return err
	}
	state, err := decodeAccountRefillState(raw)
	if err != nil || !refillHasWorkdaySchedule(state.Config) {
		return err
	}
	now := time.Now()
	if state.WorkdayCalendar != nil && now.Before(state.WorkdayCalendar.NextSyncAt) {
		return nil
	}
	fetch := s.rootService().calendarFetch
	if len(fetchers) > 0 {
		fetch = fetchers[0]
	}
	if fetch == nil {
		fetch = fetchRefillWorkdayYear
	}
	year := now.In(refillTimezone).Year()
	updates := map[int]refillWorkdayYear{}
	failures := []string{}
	nextUnpublished := false
	for _, target := range []int{year, year + 1} {
		fetchCtx, cancel := context.WithTimeout(ctx, 18*time.Second)
		data, fetchErr := fetch(fetchCtx, target)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if fetchErr != nil {
			if target == year+1 && errors.Is(fetchErr, errRefillCalendarUnpublished) {
				nextUnpublished = true
			} else {
				failures = append(failures, fmt.Sprintf("%d 年：%s", target, fetchErr))
			}
			continue
		}
		data.UpdatedAt = now
		updates[target] = data
	}
	return s.store.WithLock(ctx, func(raw string, save func(string) error) error {
		current, err := decodeAccountRefillState(raw)
		if err != nil {
			return err
		}
		if current.WorkdayCalendar == nil {
			current.WorkdayCalendar = &refillWorkdayCalendar{}
		}
		calendar := current.WorkdayCalendar
		if calendar.CheckedAt.After(now) {
			return nil // Another instance has persisted a newer refresh.
		}
		if calendar.Years == nil {
			calendar.Years = map[int]refillWorkdayYear{}
		}
		for target, data := range updates {
			calendar.Years[target] = data
		}
		for target := range calendar.Years {
			if target < year-1 || target > year+1 {
				delete(calendar.Years, target)
			}
		}
		calendar.CheckedAt, calendar.NextSyncAt = now, now.Add(24*time.Hour)
		if len(updates) > 0 {
			calendar.SyncedAt = now
		}
		calendar.Message = "中国法定工作日日历已同步，每天自动检查当年及次年安排"
		if nextUnpublished {
			calendar.Message += "；次年安排尚未发布，将持续检查"
		}
		if len(failures) > 0 {
			calendar.NextSyncAt = now.Add(time.Hour)
			calendar.Message = "日历同步未完成，保留已缓存数据，1 小时后重试：" + strings.Join(failures, "；")
		}
		refreshRefillWorkdayDeadlines(&current, time.Now())
		return saveRefillState(&current, save)
	})
}

func refreshRefillWorkdayDeadlines(state *accountRefillState, now time.Time) {
	refresh := func(schedule *AccountRefillDailySchedule, status *AccountRefillDailyScheduleStatus) {
		if schedule == nil || !schedule.Enabled || schedule.DayMode != refillChinaWorkday {
			return
		}
		next := nextRefillDailyTime(schedule, now, status.LastAt, state.WorkdayCalendar)
		today := refillDailyTime(schedule, now)
		workday, known := state.WorkdayCalendar.isWorkday(now)
		if known && workday && !status.NextAt.IsZero() && !status.NextAt.After(now) && today.After(status.LastAt) && !today.After(now) {
			next = today // A refresh cannot consume an already due batch.
		}
		status.NextAt, status.RetryAt = next, time.Time{}
		status.Message = refillDailyWaitingMessage(schedule, next)
	}
	if state.Config.DailySchedules == nil {
		refresh(state.Config.DailySchedule, &state.DailyScheduleStatus)
		return
	}
	if state.DailyScheduleStatuses == nil {
		state.DailyScheduleStatuses = map[string]AccountRefillDailyScheduleStatus{}
	}
	for _, schedule := range state.Config.DailySchedules {
		status := state.DailyScheduleStatuses[schedule.ID]
		refresh(&schedule, &status)
		state.DailyScheduleStatuses[schedule.ID] = status
	}
}
