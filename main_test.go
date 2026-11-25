package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type monitorStoreFixture struct {
	write func(context.Context, string, string, int) error
}

func (s monitorStoreFixture) SetValueWithTTL(ctx context.Context, key, value string, ttl int) error {
	return s.write(ctx, key, value, ttl)
}

func (monitorStoreFixture) SetValue(context.Context, string, string) error {
	return errors.New("fixture received a write without expiry")
}

func (monitorStoreFixture) Close() error { return nil }

func awaitResult[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("fixture did not receive a result")
		var zero T
		return zero
	}
}

func TestPublishSamplesStartsImmediatelyAndRecoversOnNextTick(t *testing.T) {
	start := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	ticks := make(chan time.Time, 1)
	times := make(chan time.Time, 2)
	times <- start.Add(time.Second)
	times <- start.Add(6 * time.Second)

	type publication struct {
		key      string
		payload  string
		ttl      int
		deadline bool
	}
	publications := make(chan publication, 2)
	first := true
	sm := &SystemMonitor{
		startTime:     start,
		containerName: "fixture",
		valkeyStore: monitorStoreFixture{write: func(ctx context.Context, key, data string, ttl int) error {
			deadline, ok := ctx.Deadline()
			publications <- publication{key, data, ttl, ok && time.Until(deadline) <= writeTimeout}
			if first {
				first = false
				return errors.New("temporary store outage")
			}
			return nil
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.publishSamples(ctx, ticks, func() time.Time { return <-times }, func() SystemMetrics {
			return SystemMetrics{CPUPercent: 12.5}
		})
	}()

	for i, age := range []int64{1, 6} {
		if i == 1 {
			ticks <- start.Add(5 * time.Second)
		}
		p := awaitResult(t, publications)
		if p.key != "system:metrics:fixture" || p.ttl != 60 || !p.deadline {
			t.Fatalf("wrong key, TTL or write deadline: %+v", p)
		}
		var sample SystemMetrics
		if err := json.Unmarshal([]byte(p.payload), &sample); err != nil {
			t.Fatal(err)
		}
		if !sample.Timestamp.Equal(start.Add(time.Duration(age)*time.Second)) ||
			!sample.CollectorStartedAt.Equal(start) || sample.CollectorUptimeSeconds != age ||
			sample.CPUPercent != 12.5 {
			t.Fatalf("unexpected observation: %+v", sample)
		}
	}
	cancel()
	awaitResult(t, done)
}

func TestObservationRetainsLegacyFieldsAndIdentifiesRestart(t *testing.T) {
	start := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	original := &SystemMonitor{containerName: "fixture", startTime: start}
	restarted := &SystemMonitor{containerName: "fixture", startTime: start.Add(15 * time.Second)}
	observed := start.Add(20 * time.Second)
	oldSample := original.observeMetrics(observed, SystemMetrics{})
	newSample := restarted.observeMetrics(observed, SystemMetrics{})

	if !oldSample.Timestamp.Equal(newSample.Timestamp) || oldSample.CollectorStartedAt.Equal(newSample.CollectorStartedAt) ||
		oldSample.CollectorUptimeSeconds != 20 || newSample.CollectorUptimeSeconds != 5 {
		t.Fatalf("restart provenance: %+v, %+v", oldSample, newSample)
	}
	data, err := json.Marshal(newSample)
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		ContainerName string    `json:"container_name"`
		Timestamp     time.Time `json:"timestamp"`
		Uptime        int64     `json:"uptime_seconds"`
		Status        string    `json:"status"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.ContainerName != "fixture" || !legacy.Timestamp.Equal(observed) || legacy.Uptime != 5 || legacy.Status != "running" {
		t.Fatalf("legacy payload changed: %+v", legacy)
	}
}

func TestMetricWriteUsesPublisherCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	results := make(chan error, 1)
	sm := &SystemMonitor{
		containerName: "fixture",
		valkeyStore: monitorStoreFixture{write: func(ctx context.Context, _, _ string, _ int) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}},
	}
	go func() {
		results <- sm.storeMetrics(ctx, SystemMetrics{})
	}()
	awaitResult(t, started)
	cancel()
	if err := awaitResult(t, results); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected publisher cancellation, got %v", err)
	}
}
