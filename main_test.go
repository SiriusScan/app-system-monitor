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
	cpuPercent := 12.5

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
			return SystemMetrics{CPUPercent: &cpuPercent}
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
			sample.ObservationVersion != observationVersion || sample.CPUPercent == nil || *sample.CPUPercent != cpuPercent {
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

func TestMemoryAvailabilityFollowsCgroupSources(t *testing.T) {
	const current = "/sys/fs/cgroup/memory.current"
	const max = "/sys/fs/cgroup/memory.max"
	zero, bytes, limit := int64(0), int64(256), int64(512)
	zeroPercent, half := 0.0, 50.0
	for _, test := range []struct {
		name    string
		files   map[string]string
		usage   *int64
		limit   *int64
		percent *float64
	}{
		{"finite denominator", map[string]string{current: "256\n", max: "512\n"}, &bytes, &limit, &half},
		{"unlimited denominator", map[string]string{current: "256", max: "max"}, &bytes, nil, nil},
		{"missing denominator", map[string]string{current: "256"}, &bytes, nil, nil},
		{"zero denominator", map[string]string{current: "256", max: "0"}, &bytes, nil, nil},
		{"measured zero", map[string]string{current: "0", max: "512"}, &zero, &limit, &zeroPercent},
		{"missing usage", map[string]string{max: "512"}, nil, &limit, nil},
		{"invalid values", map[string]string{current: "-1", max: "bad"}, nil, nil, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			read := func(path string) ([]byte, error) {
				if value, ok := test.files[path]; ok {
					return []byte(value), nil
				}
				return nil, errors.New("fixture file unavailable")
			}
			got := readMemoryInfo(read)
			if !equalOptional(got.usage, test.usage) || !equalOptional(got.limit, test.limit) ||
				!equalOptional(got.percent, test.percent) {
				t.Fatalf("unexpected memory observation: %+v", got)
			}
		})
	}
}

func equalOptional[T comparable](got, want *T) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

func TestCPUUsageRequiresAValidInterval(t *testing.T) {
	start := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	sm := &SystemMonitor{}
	for i, test := range []struct {
		usage    int64
		capacity float64
		want     *float64
	}{
		{0, 0.5, nil},
		{0, 0.5, floatPointer(0)},
		{125000, 0.5, floatPointer(25)},
		{200000, 1.0, nil}, // Changed quota starts a new baseline.
		{0, 1.0, nil},     // Reset counter starts a new baseline.
		{0, 1.0, floatPointer(0)},
	} {
		got := sm.observeCPUUsage(test.usage, test.capacity, start.Add(time.Duration(i)*time.Second))
		if !equalOptional(got, test.want) {
			t.Fatalf("observation %d disagrees with the expected CPU rate", i)
		}
	}
}

func floatPointer(value float64) *float64 { return &value }

func TestCPUCapacityAndUsageParsing(t *testing.T) {
	if got := cpuCapacity([]byte("25000 100000\n"), 8); got != 0.25 {
		t.Fatalf("fractional CPU quota: %v", got)
	}
	if got := cpuCapacity([]byte("400000 100000"), 2); got != 2 {
		t.Fatalf("process CPU availability bound: %v", got)
	}
	if got := cpuCapacity([]byte("max 100000"), 8); got != 8 {
		t.Fatalf("unlimited CPU quota: %v", got)
	}
	if got := cpuCapacity([]byte("25000 0"), 8); got != 0 {
		t.Fatalf("accepted invalid CPU period: %v", got)
	}
	if got, ok := cpuUsageUsec([]byte("user_usec 42\nusage_usec 0\n")); !ok || got != 0 {
		t.Fatalf("lost measured zero: %d, %v", got, ok)
	}
	if _, ok := cpuUsageUsec([]byte("usage_usec malformed")); ok {
		t.Fatal("accepted an invalid counter")
	}
}

func TestWireObservationDistinguishesNullFromZero(t *testing.T) {
	start := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	sm := &SystemMonitor{containerName: "fixture", startTime: start}
	rx := readCounter(func(string) ([]byte, error) { return []byte("0"), nil }, "rx")
	tx := readCounter(func(string) ([]byte, error) { return nil, errors.New("unavailable") }, "tx")
	sample := sm.observeMetrics(start, SystemMetrics{NetworkRx: rx, NetworkTx: tx})
	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"observation_version": "2", "network_rx_bytes": "0", "network_tx_bytes": "null",
		"cpu_percent": "null", "memory_usage_bytes": "null", "memory_percent": "null",
		"memory_limit_bytes": "null", "disk_usage_bytes": "null", "disk_total_bytes": "null",
		"disk_read_bytes": "null", "disk_write_bytes": "null", "disk_percent": "null",
		"file_descriptors": "null", "load_average_1m": "null",
	} {
		if got := string(wire[key]); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
