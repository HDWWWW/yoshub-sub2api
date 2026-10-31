package service

import (
	"context"
	"time"
)

const refillCapacitySampleInterval = time.Second
const refillCapacityMaxSampleGap = 2 * time.Second

type AccountRefillCapacityObservation struct {
	Status             string     `json:"status"`
	Since              *time.Time `json:"since"`
	LastSampleAt       *time.Time `json:"last_sample_at"`
	SustainedSeconds   int        `json:"sustained_seconds"`
	RequiredSeconds    int        `json:"required_seconds"`
	UtilizationPercent float64    `json:"utilization_percent"`
	Samples            int        `json:"samples"`
	Ready              bool       `json:"ready"`
}

type refillCapacityTracker struct {
	revision    int64
	groupID     int64
	target      int
	hold        int
	observation AccountRefillCapacityObservation
}

func refillCapacityHigh(c AccountRefillConfig, e AccountRefillEvaluation) bool {
	return e.Capacity > 0 && float64(e.InUse)*100 >= float64(e.Capacity)*float64(c.TargetConcurrency)
}

func (s *AccountRefillService) resetCapacityObservation() {
	s.capacityMu.Lock()
	s.capacity = refillCapacityTracker{}
	s.capacityMu.Unlock()
}

func (s *AccountRefillService) recordCapacitySample(c AccountRefillConfig, revision int64, e AccountRefillEvaluation, now time.Time) {
	s.capacityMu.Lock()
	defer s.capacityMu.Unlock()
	t := &s.capacity
	if t.revision != revision || t.groupID != c.GroupID || t.target != c.TargetConcurrency || t.hold != c.ConcurrencySustainSeconds {
		*t = refillCapacityTracker{revision: revision, groupID: c.GroupID, target: c.TargetConcurrency, hold: c.ConcurrencySustainSeconds}
	}
	o := &t.observation
	if o.LastSampleAt == nil || !now.After(*o.LastSampleAt) || now.Sub(*o.LastSampleAt) > refillCapacityMaxSampleGap {
		o.Since, o.Samples = nil, 0
	}
	o.LastSampleAt, o.RequiredSeconds = &now, c.ConcurrencySustainSeconds
	o.UtilizationPercent = 0
	if e.Capacity > 0 {
		o.UtilizationPercent = 100 * float64(e.InUse) / float64(e.Capacity)
	}
	o.Ready, o.SustainedSeconds = false, 0
	if !c.MonitorEnabled || c.Mode != "capacity" || !refillInWindow(c, now) || !refillCapacityHigh(c, e) {
		o.Status, o.Since, o.Samples = "below", nil, 0
		return
	}
	if o.Since == nil {
		o.Since = &now
	}
	o.Samples++
	o.SustainedSeconds = int(now.Sub(*o.Since) / time.Second)
	o.Ready = o.Samples >= 2 && o.SustainedSeconds >= c.ConcurrencySustainSeconds
	o.Status = "observing"
	if o.Ready {
		o.Status = "ready"
	}
}

func (s *AccountRefillService) capacityObservation(c AccountRefillConfig, revision int64, now time.Time) AccountRefillCapacityObservation {
	s.capacityMu.Lock()
	defer s.capacityMu.Unlock()
	t := s.capacity
	if !c.MonitorEnabled || c.Mode != "capacity" || !refillInWindow(c, now) {
		return AccountRefillCapacityObservation{Status: "disabled", RequiredSeconds: c.ConcurrencySustainSeconds}
	}
	if t.revision != revision || t.groupID != c.GroupID || t.target != c.TargetConcurrency || t.hold != c.ConcurrencySustainSeconds ||
		t.observation.LastSampleAt == nil || now.Before(*t.observation.LastSampleAt) || now.Sub(*t.observation.LastSampleAt) > refillCapacityMaxSampleGap {
		return AccountRefillCapacityObservation{Status: "waiting", RequiredSeconds: c.ConcurrencySustainSeconds}
	}
	return t.observation
}

// Sampling runs independently of provider requests and the purchase check interval.
// Missing data, pauses, process restarts and changed settings require a new window.
func (s *AccountRefillService) sampleCapacity(ctx context.Context) {
	raw, err := s.store.Read(ctx)
	if err != nil || raw == "" {
		s.resetCapacityObservation()
		return
	}
	state, err := decodeAccountRefillState(raw)
	if err != nil || !state.Config.MonitorEnabled || state.Config.Mode != "capacity" || state.Config.GroupID <= 0 {
		s.resetCapacityObservation()
		return
	}
	if err := s.evaluate(ctx, &state, time.Now()); err != nil {
		s.resetCapacityObservation()
		return
	}
	s.recordCapacitySample(state.Config, state.Revision, state.Evaluation, time.Now())
}

func (s *AccountRefillService) ImportExpiryGraceMinutes(ctx context.Context) int {
	raw, err := s.store.Read(ctx)
	if err == nil {
		state, decodeErr := decodeAccountRefillState(raw)
		if decodeErr == nil && state.Config.ImportExpiryGraceMinutes >= 0 && state.Config.ImportExpiryGraceMinutes <= 1440 {
			return state.Config.ImportExpiryGraceMinutes
		}
	}
	return AccountImportExpiryOffsetMinutes
}
