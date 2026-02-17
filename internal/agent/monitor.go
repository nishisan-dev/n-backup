package agent

import (
	"log/slog"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
)

// SystemStats holds collected system metrics.
type SystemStats struct {
	CPUPercent       float64
	MemoryPercent    float64
	DiskUsagePercent float64
	LoadAverage      float64
}

// SystemMonitor collects system metrics periodically.
type SystemMonitor struct {
	logger *slog.Logger
	close  chan struct{}
	wg     sync.WaitGroup
	stats  SystemStats
	mu     sync.RWMutex
}

// NewSystemMonitor creates a new SystemMonitor.
func NewSystemMonitor(logger *slog.Logger) *SystemMonitor {
	return &SystemMonitor{
		logger: logger.With("component", "system_monitor"),
		close:  make(chan struct{}),
	}
}

// Start begins periodic metric collection.
func (sm *SystemMonitor) Start() {
	sm.wg.Add(1)
	go sm.run()
}

// Stop stops the monitor.
func (sm *SystemMonitor) Stop() {
	close(sm.close)
	sm.wg.Wait()
}

// Stats returns the latest collected stats.
func (sm *SystemMonitor) Stats() SystemStats {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.stats
}

func (sm *SystemMonitor) run() {
	defer sm.wg.Done()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	// Initial collection
	sm.collect()

	for {
		select {
		case <-sm.close:
			return
		case <-ticker.C:
			sm.collect()
		}
	}
}

func (sm *SystemMonitor) collect() {
	stats := SystemStats{}

	// CPU
	if percentage, err := cpu.Percent(0, false); err == nil && len(percentage) > 0 {
		stats.CPUPercent = percentage[0]
	} else {
		sm.logger.Debug("failed to collect cpu stats", "error", err)
	}

	// Memory
	if v, err := mem.VirtualMemory(); err == nil {
		stats.MemoryPercent = v.UsedPercent
	} else {
		sm.logger.Debug("failed to collect memory stats", "error", err)
	}

	// Disk (Root)
	if d, err := disk.Usage("/"); err == nil {
		stats.DiskUsagePercent = d.UsedPercent
	} else {
		sm.logger.Debug("failed to collect disk stats", "error", err)
	}

	// Load Avg
	if l, err := load.Avg(); err == nil {
		stats.LoadAverage = l.Load1
	} else {
		sm.logger.Debug("failed to collect load stats", "error", err)
	}

	sm.mu.Lock()
	sm.stats = stats
	sm.mu.Unlock()
}
