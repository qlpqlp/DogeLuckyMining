package main

import (
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"
)

// MinerOptimizer automatically tunes mining parameters for optimal performance on Lucky Mining
type MinerOptimizer struct {
	miner              *Miner
	optimizationTicker *time.Ticker
	stopChan           chan struct{}
	mu                 sync.RWMutex
	currentProfile     OptimizationProfile
	hashRateHistory    []float64
	lastOptimizeTime   time.Time
}

// OptimizationProfile represents the current optimal mining configuration
type OptimizationProfile struct {
	ThreadCount      int
	MaxHashes        uint64
	MiningIntensity  string
	DeviceType       string // "cpu" or "gpu"
	UseStride        bool
	HashRateEstimate float64
	Timestamp        time.Time
	Reason           string // Why this profile was chosen
}

// NewMinerOptimizer creates a new optimizer
func NewMinerOptimizer(miner *Miner) *MinerOptimizer {
	return &MinerOptimizer{
		miner:           miner,
		stopChan:        make(chan struct{}),
		hashRateHistory: make([]float64, 0, 10),
	}
}

// Start begins the optimization loop
func (o *MinerOptimizer) Start() {
	go func() {
		// Panic recovery for optimizer goroutine
		defer func() {
			if r := recover(); r != nil {
				log.Printf("⚠️ Optimizer panic (recovered): %v", r)
			}
		}()

		// Detect system capabilities (just for logging, don't apply yet)
		o.detectSystemCapabilities()

		// Wait for mining to stabilize (2 seconds) before starting optimization loop
		// This avoids race conditions during mining startup
		time.Sleep(2 * time.Second)

		// Log that optimizer is ready
		log.Printf("⚙️ AUTO-OPTIMIZER: Ready - will monitor and adjust every 2 minutes")

		// Start optimization ticker
		o.optimizationTicker = time.NewTicker(2 * time.Minute)
		defer o.optimizationTicker.Stop()

		for {
			select {
			case <-o.stopChan:
				return
			case <-o.optimizationTicker.C:
				o.optimizeConfiguration()
			}
		}
	}()
}

// Stop stops the optimizer
func (o *MinerOptimizer) Stop() {
	select {
	case <-o.stopChan:
	default:
		close(o.stopChan)
	}
}

// detectSystemCapabilities analyzes system and sets initial profile
func (o *MinerOptimizer) detectSystemCapabilities() {
	log.Printf("🔍 AUTO-OPTIMIZER: Detecting system capabilities...")

	cpuCores := runtime.NumCPU()
	log.Printf("   CPU cores detected: %d", cpuCores)

	// Determine optimal thread count based on CPU cores
	// Strategy: Use all cores for high-end systems, be conservative for low-end
	optimalThreads := cpuCores
	if cpuCores > 8 {
		// For high-end systems, we can use all cores
		optimalThreads = cpuCores
	} else if cpuCores > 4 {
		// Mid-range: use 75% of cores
		optimalThreads = (cpuCores * 3) / 4
	} else {
		// Low-end: use 50% to leave headroom
		optimalThreads = cpuCores / 2
		if optimalThreads == 0 {
			optimalThreads = 1
		}
	}

	// Determine mining intensity based on CPU cores
	var intensity string
	var maxHashes uint64
	if cpuCores >= 16 {
		intensity = "extreme"
		maxHashes = 1000000
	} else if cpuCores >= 8 {
		intensity = "high"
		maxHashes = 500000
	} else if cpuCores >= 4 {
		intensity = "medium"
		maxHashes = 100000
	} else {
		intensity = "low"
		maxHashes = 50000
	}

	// Check for GPU availability - prefer GPU if available, fall back to CPU
	deviceType := "cpu"
	if IsGPUAvailable() {
		deviceType = "gpu"
		log.Printf("🎮 GPU detected! Using GPU for mining (faster for Scrypt)")
	} else {
		log.Printf("💻 No GPU detected - using CPU (OpenCL not available)")
	}

	profile := OptimizationProfile{
		ThreadCount:     optimalThreads,
		MaxHashes:       maxHashes,
		MiningIntensity: intensity,
		DeviceType:      deviceType,
		UseStride:       false, // Range-based is more standard
		Timestamp:       time.Now(),
		Reason:          fmt.Sprintf("System detection: %d CPU cores", cpuCores),
	}

	log.Printf("✅ AUTO-OPTIMIZER: Initial profile selected")
	log.Printf("   Threads: %d, Intensity: %s, MaxHashes: %d", profile.ThreadCount, profile.MiningIntensity, profile.MaxHashes)

	modeStr := "range"
	if profile.UseStride {
		modeStr = "stride"
	}
	log.Printf("   Device: %s, Mode: %s", profile.DeviceType, modeStr)

	// Store initial profile without applying yet (mining loop will pick it up)
	o.mu.Lock()
	o.currentProfile = profile
	o.mu.Unlock()
}

// optimizeConfiguration runs optimization checks
func (o *MinerOptimizer) optimizeConfiguration() {
	o.mu.RLock()
	miner := o.miner
	o.mu.RUnlock()

	if miner == nil {
		return
	}

	// Get current mining stats
	miner.statsMutex.RLock()
	currentHashRate := miner.stats.HashRate
	isRunning := miner.stats.Status == "running"
	miner.statsMutex.RUnlock()

	if !isRunning || currentHashRate < 100 {
		// Can't optimize if not running or hash rate is too low
		return
	}

	log.Printf("📊 AUTO-OPTIMIZER: Analyzing performance (Hash Rate: %.0f H/s)...", currentHashRate)

	// Track hash rate history
	o.mu.Lock()
	o.hashRateHistory = append(o.hashRateHistory, currentHashRate)
	if len(o.hashRateHistory) > 10 {
		o.hashRateHistory = o.hashRateHistory[1:]
	}
	history := append([]float64{}, o.hashRateHistory...)
	o.mu.Unlock()

	// Analyze trends
	recommendation := o.analyzePerformance(history)
	if recommendation != nil {
		log.Printf("💡 AUTO-OPTIMIZER: Recommendation - %s", recommendation.Reason)
		o.applyProfile(*recommendation)
	}
}

// analyzePerformance analyzes hash rate trends and suggests improvements
func (o *MinerOptimizer) analyzePerformance(history []float64) *OptimizationProfile {
	if len(history) < 3 {
		return nil // Not enough data
	}

	// Calculate average and trend
	avg := 0.0
	for _, h := range history {
		avg += h
	}
	avg /= float64(len(history))

	// Check for declining performance
	recentAvg := avg
	if len(history) > 5 {
		recentAvg = 0.0
		for _, h := range history[len(history)-3:] {
			recentAvg += h
		}
		recentAvg /= 3.0
	}

	// If hash rate is declining, try adjusting
	if recentAvg < avg*0.8 {
		// Performance declining - maybe adjust thread count
		log.Printf("⚠️ AUTO-OPTIMIZER: Hash rate declining (%.0f → %.0f)", avg, recentAvg)
		return o.suggestAdjustment()
	}

	// NOTE: Do NOT auto-suggest GPU/CPU switching - that's a user choice
	// Users should manually select their preferred device
	// The optimizer only tunes ThreadCount, MaxHashes, and MiningIntensity

	// If hash rate is very high and stable, we might push harder
	if avg > 500000 && len(history) > 5 {
		// System is performing well, try increasing intensity
		return o.suggestIntensityIncrease()
	}

	return nil
}

// suggestAdjustment suggests configuration changes when performance drops
func (o *MinerOptimizer) suggestAdjustment() *OptimizationProfile {
	o.mu.RLock()
	current := o.currentProfile
	o.mu.RUnlock()

	// Try reducing threads first (might help CPU stay cooler)
	if current.ThreadCount > 1 {
		newThreads := current.ThreadCount - 1
		return &OptimizationProfile{
			ThreadCount:     newThreads,
			MaxHashes:       current.MaxHashes,
			MiningIntensity: current.MiningIntensity,
			DeviceType:      current.DeviceType,
			UseStride:       current.UseStride,
			Timestamp:       time.Now(),
			Reason:          fmt.Sprintf("Hash rate declining - reduced threads from %d to %d", current.ThreadCount, newThreads),
		}
	}

	return nil
}

// suggestIntensityIncrease suggests pushing harder when performance is good
func (o *MinerOptimizer) suggestIntensityIncrease() *OptimizationProfile {
	o.mu.RLock()
	current := o.currentProfile
	o.mu.RUnlock()

	// Try increasing intensity if not already at max
	var newIntensity string
	var newHashes uint64
	changed := false

	switch current.MiningIntensity {
	case "low":
		newIntensity = "medium"
		newHashes = 100000
		changed = true
	case "medium":
		newIntensity = "high"
		newHashes = 500000
		changed = true
	case "high":
		newIntensity = "extreme"
		newHashes = 1000000
		changed = true
	}

	if changed {
		return &OptimizationProfile{
			ThreadCount:     current.ThreadCount,
			MaxHashes:       newHashes,
			MiningIntensity: newIntensity,
			DeviceType:      current.DeviceType,
			UseStride:       current.UseStride,
			Timestamp:       time.Now(),
			Reason:          fmt.Sprintf("Performance excellent - increased intensity from %s to %s", current.MiningIntensity, newIntensity),
		}
	}

	return nil
}

// suggestSwitchToGPU suggests switching to GPU if available
func (o *MinerOptimizer) suggestSwitchToGPU() *OptimizationProfile {
	o.mu.RLock()
	current := o.currentProfile
	o.mu.RUnlock()

	return &OptimizationProfile{
		ThreadCount:     current.ThreadCount,
		MaxHashes:       current.MaxHashes,
		MiningIntensity: current.MiningIntensity,
		DeviceType:      "gpu",
		UseStride:       current.UseStride,
		Timestamp:       time.Now(),
		Reason:          "Switching to GPU for significantly faster Scrypt hashing",
	}
}

// applyProfile applies an optimization profile to the miner
func (o *MinerOptimizer) applyProfile(profile OptimizationProfile) {
	o.mu.Lock()
	o.currentProfile = profile
	o.lastOptimizeTime = time.Now()
	o.mu.Unlock()

	// Update miner configuration
	miner := o.miner
	if miner == nil {
		log.Printf("⚠️ AUTO-OPTIMIZER: Cannot apply profile - miner is nil")
		return
	}

	if miner.config == nil {
		log.Printf("⚠️ AUTO-OPTIMIZER: Cannot apply profile - config is nil")
		return
	}

	miner.configMutex.Lock()
	defer miner.configMutex.Unlock()

	// Safely update config
	// NOTE: Do NOT override DeviceType - that's a user choice, not an auto-optimizer parameter
	miner.config.ThreadCount = profile.ThreadCount
	miner.config.MaxHashes = profile.MaxHashes
	miner.config.MiningIntensity = profile.MiningIntensity
	// miner.config.DeviceType = profile.DeviceType  // USER CHOICE - DO NOT OVERRIDE
	miner.config.UseStride = profile.UseStride

	// Also update the actual miner if it exists
	if miner.miner != nil {
		miner.miner.ThreadCount = profile.ThreadCount
		miner.miner.MaxHashes = profile.MaxHashes
		miner.miner.DebugLogging = false // Keep debug off during auto-optimization
		miner.miner.UseStride = profile.UseStride
	}

	// Log the change
	log.Printf("⚙️ AUTO-OPTIMIZER: Applied profile")
	log.Printf("   Threads: %d, Intensity: %s, MaxHashes: %d", profile.ThreadCount, profile.MiningIntensity, profile.MaxHashes)
	log.Printf("   Reason: %s", profile.Reason)

	// Notify UI (after unlocking to avoid deadlock)
	if miner != nil {
		miner.broadcastLogMessage(fmt.Sprintf("⚙️ Auto-optimizer: %s", profile.Reason))
	}
}

// GetProfile returns current optimization profile
func (o *MinerOptimizer) GetProfile() OptimizationProfile {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.currentProfile
}

// GetCurrentHashRate estimates current hash rate
func (o *MinerOptimizer) GetCurrentHashRate() float64 {
	o.mu.RLock()
	if len(o.hashRateHistory) == 0 {
		o.mu.RUnlock()
		return 0
	}
	lastRate := o.hashRateHistory[len(o.hashRateHistory)-1]
	o.mu.RUnlock()
	return lastRate
}
