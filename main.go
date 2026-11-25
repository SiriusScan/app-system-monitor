package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SiriusScan/go-api/sirius/store"
)

const (
	observationVersion = 2
	sampleInterval     = 5 * time.Second
	metricsTTL         = 60 * time.Second
	writeTimeout       = 2 * time.Second
)

// SystemMetrics is an observation made by the collector. A nil measurement
// is encoded as null. Uptime and Status describe the collector process.
type SystemMetrics struct {
	ObservationVersion     int       `json:"observation_version"`
	ContainerName          string    `json:"container_name"`
	Timestamp              time.Time `json:"timestamp"`
	CollectorStartedAt     time.Time `json:"collector_started_at"`
	CollectorUptimeSeconds int64     `json:"collector_uptime_seconds"`
	CPUPercent             *float64  `json:"cpu_percent"`
	MemoryUsage            *int64    `json:"memory_usage_bytes"`
	MemoryLimit            *int64    `json:"memory_limit_bytes"`
	MemoryPercent          *float64  `json:"memory_percent"`
	NetworkRx              *int64    `json:"network_rx_bytes"`
	NetworkTx              *int64    `json:"network_tx_bytes"`
	DiskRead               *int64    `json:"disk_read_bytes"`
	DiskWrite              *int64    `json:"disk_write_bytes"`
	DiskUsage              *int64    `json:"disk_usage_bytes"`
	DiskTotal              *int64    `json:"disk_total_bytes"`
	DiskPercent            *float64  `json:"disk_percent"`
	ProcessCount           *int      `json:"process_count"`
	FileDescriptors        *int      `json:"file_descriptors"`
	LoadAverage1m          *float64  `json:"load_average_1m"`
	LoadAverage5m          *float64  `json:"load_average_5m"`
	LoadAverage15m         *float64  `json:"load_average_15m"`
	Uptime                 int64     `json:"uptime_seconds"`
	Status                 string    `json:"status"`
}

// ContainerLog represents a container log entry
type ContainerLog struct {
	ContainerName string    `json:"container_name"`
	Timestamp     time.Time `json:"timestamp"`
	Level         string    `json:"level"`
	Message       string    `json:"message"`
}

type monitorStore interface {
	SetValue(context.Context, string, string) error
	SetValueWithTTL(context.Context, string, string, int) error
	Close() error
}

// SystemMonitor handles system metrics collection and reporting.
type SystemMonitor struct {
	containerName   string
	valkeyStore     monitorStore
	logsChan        chan ContainerLog
	startTime       time.Time
	lastCPUUsage    int64
	lastCPUCapacity float64
	lastCPUTime     time.Time
}

// NewSystemMonitor creates a new system monitor instance
func NewSystemMonitor(containerName string, startedAt time.Time) (*SystemMonitor, error) {
	// Initialize Valkey store
	valkeyStore, err := store.NewValkeyStore()
	if err != nil {
		return nil, fmt.Errorf("failed to create Valkey store: %w", err)
	}

	return &SystemMonitor{
		containerName: containerName,
		valkeyStore:   valkeyStore,
		logsChan:      make(chan ContainerLog, 100),
		startTime:     startedAt,
	}, nil
}

// Start begins the system monitoring process
func (sm *SystemMonitor) Start(ctx context.Context) error {
	log.Printf("Starting system monitor for container: %s", sm.containerName)

	var workers sync.WaitGroup
	for _, run := range []func(context.Context){sm.collectMetrics, sm.collectLogs, sm.reportLogs} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			run(ctx)
		}()
	}
	workers.Wait()
	log.Printf("System monitor stopping for container: %s", sm.containerName)
	return nil
}

// collectMetrics samples immediately and subsequently on the normal cadence.
func (sm *SystemMonitor) collectMetrics(ctx context.Context) {
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()

	sm.publishSamples(ctx, ticker.C, time.Now, sm.gatherSystemMetrics)
}

func (sm *SystemMonitor) publishSamples(
	ctx context.Context, ticks <-chan time.Time, now func() time.Time, gather func() SystemMetrics,
) {
	for ctx.Err() == nil {
		metrics := gather()
		if err := sm.storeMetrics(ctx, sm.observeMetrics(now(), metrics)); err != nil && ctx.Err() == nil {
			log.Printf("Could not publish metrics for %s", sm.containerName)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

func (sm *SystemMonitor) observeMetrics(observedAt time.Time, metrics SystemMetrics) SystemMetrics {
	uptime := int64(max(observedAt.Sub(sm.startTime), 0) / time.Second)
	metrics.ObservationVersion = observationVersion
	metrics.ContainerName = sm.containerName
	metrics.Timestamp = observedAt.UTC()
	metrics.CollectorStartedAt = sm.startTime.UTC()
	metrics.CollectorUptimeSeconds = uptime
	metrics.Uptime = uptime
	metrics.Status = "running"
	return metrics
}

// collectLogs collects container logs
func (sm *SystemMonitor) collectLogs(ctx context.Context) {
	// For now, we'll generate some sample logs
	// In a real implementation, this would read from container log files
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	logMessages := []string{
		"System monitor running normally",
		"Metrics collection active",
		"Valkey connection healthy",
		"Container status: running",
	}

	messageIndex := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logEntry := ContainerLog{
				ContainerName: sm.containerName,
				Timestamp:     time.Now(),
				Level:         "INFO",
				Message:       logMessages[messageIndex%len(logMessages)],
			}

			select {
			case sm.logsChan <- logEntry:
			default:
				log.Printf("Warning: logs channel full, dropping log")
			}

			messageIndex++
		}
	}
}

// reportLogs reports logs to Valkey
func (sm *SystemMonitor) reportLogs(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case logEntry := <-sm.logsChan:
			if err := sm.storeLog(ctx, logEntry); err != nil && ctx.Err() == nil {
				log.Printf("Error storing log: %v", err)
			}
		}
	}
}

// gatherSystemMetrics reads resource counters in the collector's container.
func (sm *SystemMonitor) gatherSystemMetrics() SystemMetrics {
	memory := readMemoryInfo(os.ReadFile)
	return SystemMetrics{
		CPUPercent:    sm.readCPUInfo(),
		MemoryUsage:   memory.usage,
		MemoryLimit:   memory.limit,
		MemoryPercent: memory.percent,
		NetworkRx:     readCounter(os.ReadFile, "/sys/class/net/eth0/statistics/rx_bytes"),
		NetworkTx:     readCounter(os.ReadFile, "/sys/class/net/eth0/statistics/tx_bytes"),
		ProcessCount:  readProcessCount(),
	}
}

type memoryObservation struct {
	usage   *int64
	limit   *int64
	percent *float64
}

func readMemoryInfo(readFile func(string) ([]byte, error)) memoryObservation {
	memory := memoryObservation{
		usage: readCounter(readFile, "/sys/fs/cgroup/memory.current"),
		limit: readCounter(readFile, "/sys/fs/cgroup/memory.max"),
	}
	if memory.limit != nil && *memory.limit == 0 {
		memory.limit = nil
	}
	// memory.max can be "max" on an unlimited cgroup. The current usage
	// remains a measurement even when there is no finite denominator.
	if memory.usage != nil && memory.limit != nil {
		percent := float64(*memory.usage) / float64(*memory.limit) * 100
		memory.percent = &percent
	}
	return memory
}

func readCounter(readFile func(string) ([]byte, error), path string) *int64 {
	data, err := readFile(path)
	if err != nil {
		return nil
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || value < 0 {
		return nil
	}
	return &value
}

func (sm *SystemMonitor) readCPUInfo() *float64 {
	stat, statErr := os.ReadFile("/sys/fs/cgroup/cpu.stat")
	quota, quotaErr := os.ReadFile("/sys/fs/cgroup/cpu.max")
	usage, measured := cpuUsageUsec(stat)
	capacity := cpuCapacity(quota, runtime.NumCPU())
	if statErr != nil || quotaErr != nil || !measured || capacity <= 0 {
		sm.lastCPUTime = time.Time{}
		return nil
	}
	return sm.observeCPUUsage(usage, capacity, time.Now())
}

func cpuUsageUsec(stat []byte) (int64, bool) {
	for _, line := range strings.Split(string(stat), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "usage_usec" {
			continue
		}
		usage, err := strconv.ParseInt(fields[1], 10, 64)
		return usage, err == nil && usage >= 0
	}
	return 0, false
}

// cpuCapacity returns the cgroup quota in logical CPUs, bounded by the CPUs
// available to the process at startup. A zero capacity denotes an invalid input.
func cpuCapacity(quota []byte, availableCPUs int) float64 {
	fields := strings.Fields(string(quota))
	if len(fields) != 2 || availableCPUs <= 0 {
		return 0
	}
	period, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || period <= 0 {
		return 0
	}
	if fields[0] == "max" {
		return float64(availableCPUs)
	}
	limit, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || limit <= 0 {
		return 0
	}
	return min(float64(availableCPUs), float64(limit)/float64(period))
}

func (sm *SystemMonitor) observeCPUUsage(usage int64, capacity float64, observedAt time.Time) *float64 {
	previousUsage, previousCapacity, previousTime := sm.lastCPUUsage, sm.lastCPUCapacity, sm.lastCPUTime
	sm.lastCPUUsage, sm.lastCPUCapacity, sm.lastCPUTime = usage, capacity, observedAt
	if previousTime.IsZero() || !observedAt.After(previousTime) || usage < previousUsage || capacity != previousCapacity {
		return nil
	}

	elapsedSeconds := observedAt.Sub(previousTime).Seconds()
	usedSeconds := float64(usage-previousUsage) / 1_000_000
	percent := usedSeconds / elapsedSeconds / capacity * 100
	return &percent
}

// readProcessCount counts process entries visible in the container's /proc.
func readProcessCount() *int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			if _, err := strconv.Atoi(entry.Name()); err == nil {
				count++
			}
		}
	}
	return &count
}

// storeMetrics stores metrics in Valkey
func (sm *SystemMonitor) storeMetrics(ctx context.Context, metrics SystemMetrics) error {
	key := fmt.Sprintf("system:metrics:%s", sm.containerName)

	data, err := json.Marshal(metrics)
	if err != nil {
		return fmt.Errorf("failed to marshal metrics: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return sm.valkeyStore.SetValueWithTTL(ctx, key, string(data), int(metricsTTL/time.Second))
}

// storeLog stores log entry in Valkey
func (sm *SystemMonitor) storeLog(ctx context.Context, logEntry ContainerLog) error {
	key := fmt.Sprintf("system:logs:%s:%d", sm.containerName, time.Now().UnixNano())

	data, err := json.Marshal(logEntry)
	if err != nil {
		return fmt.Errorf("failed to marshal log: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return sm.valkeyStore.SetValue(ctx, key, string(data))
}

func main() {
	startedAt := time.Now()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	containerName := os.Getenv("CONTAINER_NAME")
	if containerName == "" {
		containerName = "unknown"
	}

	retry := time.NewTicker(sampleInterval)
	defer retry.Stop()
	for ctx.Err() == nil {
		monitor, err := NewSystemMonitor(containerName, startedAt)
		if err == nil {
			defer monitor.valkeyStore.Close()
			if err := monitor.Start(ctx); err != nil {
				log.Printf("System monitor failed: %v", err)
			}
			return
		}

		log.Print("Valkey unavailable at startup. Retrying system monitor connection.")
		select {
		case <-ctx.Done():
			return
		case <-retry.C:
		}
	}
}
