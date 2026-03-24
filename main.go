package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dogeorg/doge"
	"github.com/gorilla/websocket"
)

//go:embed web/index.html web/static/*
var embeddedWeb embed.FS

// Coded with love for the Dogecoin community — open source.
// Author: Paulo Vidal (Dogecoin Foundation Dev)
// Social: x.com/inevitable360 | GitHub: https://github.com/qlpqlp

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	configFileName      = "config.json"
	chainTipFileName    = "chain_tip.json"
	hadConfigAtStartup  bool // true if config file existed on startup (so we don't show first-run modal)
	logCapture          *logCaptureBuffer
	p2pForceGenesisNext bool // when true, next P2P sync uses genesis (set after chain validation failure)
)

const maxLogLines = 400

// logCaptureBuffer captures log output for the web UI (same as console).
type logCaptureBuffer struct {
	mu    sync.RWMutex
	lines []string
}

func (b *logCaptureBuffer) Write(p []byte) (n int, err error) {
	scanner := bufio.NewScanner(bytes.NewReader(p))
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\n")
		if line == "" {
			continue
		}
		b.mu.Lock()
		b.lines = append(b.lines, line)
		if len(b.lines) > maxLogLines {
			b.lines = b.lines[len(b.lines)-maxLogLines:]
		}
		b.mu.Unlock()
	}
	return len(p), nil
}

func (b *logCaptureBuffer) Lines() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}

type Miner struct {
	rpcClient         *RPCClient
	p2pClient         *P2PClient
	miner             *ScryptMiner
	gpuMiner          *GPUMiner
	headerFetcher     *HeaderFetcher
	paymentMonitor    *P2PPaymentMonitor // P2P-based payment tracking (no APIs)
	stats             *MiningStats
	statsMutex        sync.RWMutex
	config            *MinerConfig
	configMutex       sync.RWMutex
	stopChan          chan struct{}
	wg                sync.WaitGroup
	clients           map[*websocket.Conn]bool
	clientsMutex      sync.RWMutex
	wsWriteMutex      sync.Mutex // Protects websocket writes (gorilla websocket is not thread-safe)
	enableHashLogging bool       // Toggle for hash attempt logging
	logMutex          sync.RWMutex
	extraNonce        uint32           // Sequential extra nonce counter (like real miners)
	extraNonceMutex   sync.Mutex       // Protects extraNonce increment
	submittedBlocks   []SubmittedBlock // Track submitted blocks
	submittedMutex    sync.RWMutex
	optimizer         *MinerOptimizer  // Auto-optimizer for Lucky mining
}

type MinerConfig struct {
	RPCURL     string `json:"rpcUrl"`
	RPCUser    string `json:"rpcUser"`
	RPCPass    string `json:"rpcPass"`
	PayoutAddr string `json:"payoutAddr"` // active address for current network (used by miner)
	// Per-network payout addresses so mainnet/testnet each keep their own
	PayoutAddrMainnet string `json:"payoutAddrMainnet"`
	PayoutAddrTestnet string `json:"payoutAddrTestnet"`
	// Network: "mainnet" (default) or "testnet"
	Network string `json:"network"`
	// Connection mode:
	// - "rpc" (default): use Dogecoin Core RPC for chain tip + block submission
	// - "p2p": connect directly to Dogecoin peers (no local Core node required)
	ConnectionMode string `json:"connectionMode"`
	// P2P: optional checkpoint id to start header sync from (same as dogecoin-wallet; e.g. "0", "5050000")
	P2PCheckpoint string `json:"p2pCheckpoint"`
	// P2P: optional peer override (e.g. "1.2.3.4:44556" for testnet). When set, try this peer first; use when the DNS seed keeps disconnecting.
	P2PPeerOverride        string `json:"p2pPeerOverride"`        // Legacy: applies to current network (deprecated, use network-specific fields)
	P2PPeerOverrideMainnet string `json:"p2pPeerOverrideMainnet"` // Mainnet-specific peer override (e.g. "seed.multidoge.org:22556")
	P2PPeerOverrideTestnet string `json:"p2pPeerOverrideTestnet"` // Testnet-specific peer override (e.g. "testseed.jrn.me.uk:44556")
	// Mining performance settings
	DeviceType      string `json:"deviceType"`      // "cpu" (optimized parallel CPU mining)
	ThreadCount     int    `json:"threadCount"`     // Number of parallel mining threads (1-16)
	MaxHashes       uint64 `json:"maxHashes"`       // Max hashes per block template (1000-1000000)
	MiningIntensity string `json:"miningIntensity"` // "low", "medium", "high", "extreme"
	// Nonce search strategy settings
	NonceSearchMode string `json:"nonceSearchMode"` // "forward", "backward", "sequential" (default)
	NonceStartMode  string `json:"nonceStartMode"`  // "random", "calculated" (from block metadata)
	UseStride       bool   `json:"useStride"`       // Use stride-based iteration for multi-threading
	// Debug logging
	DebugLogging    bool   `json:"debugLogging"`    // Enable debug logging (verbose output)
}

type SubmittedBlock struct {
	Height       int64     `json:"height"`
	Hash         string    `json:"hash"`
	Nonce        uint32    `json:"nonce"`
	SubmitTime   time.Time `json:"submit_time"`
	IsStale      bool      `json:"is_stale"`
	BroadcastOk  bool      `json:"broadcast_ok"`
	PeerCount    int       `json:"peer_count"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

type MiningStats struct {
	HashRate                float64          `json:"hashRate"`
	TotalHashes             uint64           `json:"totalHashes"`
	BlocksFound             uint64           `json:"blocksFound"`
	BlocksAccepted          uint64           `json:"blocksAccepted"` // Blocks successfully broadcast
	BlocksStale             uint64           `json:"blocksStale"`    // Blocks detected as stale
	SharesSubmitted         uint64           `json:"sharesSubmitted"`
	StartTime               time.Time        `json:"startTime"`
	CurrentBlock            *BlockInfo       `json:"currentBlock"`
	Status                  string           `json:"status"`
	LastUpdate              time.Time        `json:"lastUpdate"`
	LogMessages             []string         `json:"logMessages"`               // Recent log messages for web interface
	P2PPeer                 string           `json:"p2pPeer"`                   // P2P mode: connected peer address
	P2PPeerInfo             *PeerInfo        `json:"p2pPeerInfo,omitempty"`     // P2P peer version/subversion from version message
	Network                 string           `json:"network"`                   // "mainnet" or "testnet"
	PayoutAddress           string           `json:"payoutAddress"`             // Current payout address
	PaymentTracking         *P2PMonitorStats `json:"paymentTracking,omitempty"` // P2P payment tracking (no APIs)
	ExpectedSecondsPerBlock float64          `json:"expectedSecondsPerBlock"`   // At current diff/hashrate (0 = unknown)
	BlockAttemptSeconds     float64          `json:"blockAttemptSeconds"`       // Time spent mining last block attempt (for monitoring)
	DeviceType              string           `json:"deviceType"`                // "cpu" or "gpu"
}

type BlockInfo struct {
	Height           int64   `json:"height"`
	Hash             string  `json:"hash"`
	Difficulty       float64 `json:"difficulty"`
	Target           string  `json:"target"`
	HashesAttempted  uint64  `json:"hashesAttempted"`  // Hashes tried for current block (resets per block)
}

func NewMiner(rpcURL, rpcUser, rpcPass, payoutAddr string) (*Miner, error) {
	// Always check for GPU on startup
	log.Printf("🔍 Checking for GPU availability...")
	deviceType := "cpu"
	if IsGPUAvailable() {
		deviceType = "gpu"
		log.Printf("🎮 GPU detected - mining will use GPU (OpenCL accelerated)")
	} else {
		log.Printf("💻 GPU not available - mining will use CPU")
	}

	config := &MinerConfig{
		RPCURL:          rpcURL,
		RPCUser:         rpcUser,
		RPCPass:         rpcPass,
		PayoutAddr:      payoutAddr,
		Network:         "mainnet",    // Default to mainnet
		ConnectionMode:  "rpc",        // Default to RPC mode
		DeviceType:      deviceType,   // Auto-detected: GPU if available, CPU otherwise
		ThreadCount:     1,            // Default to single thread
		MaxHashes:       100000,       // Default max hashes per block
		MiningIntensity: "medium",     // Default intensity
		NonceSearchMode: "sequential", // Default: sequential from 0
		NonceStartMode:  "calculated", // Default: calculate from block metadata
		UseStride:       false,        // Default: range-based (not stride)
		DebugLogging:    false,        // Default: no debug logging (reduce spam)
	}

	var rpcClient *RPCClient
	var p2pClient *P2PClient
	var headerFetcher *HeaderFetcher

	// Initialize connectivity based on mode
	if config.Network == "" {
		config.Network = "mainnet"
	}
	switch config.ConnectionMode {
	case "p2p":
		// Pure P2P mode: no local Dogecoin Core required
		p2pClient = NewP2PClient(config.Network)
		// Use network-specific peer override, falling back to legacy field
		peerOverride := config.P2PPeerOverride // legacy
		if config.Network == "testnet" && config.P2PPeerOverrideTestnet != "" {
			peerOverride = config.P2PPeerOverrideTestnet
		} else if config.Network == "mainnet" && config.P2PPeerOverrideMainnet != "" {
			peerOverride = config.P2PPeerOverrideMainnet
		}
		if peerOverride != "" {
			p2pClient.SetPeerOverride(peerOverride)
		}
	default:
		// Default to RPC mode
		config.ConnectionMode = "rpc"
		rpcClient = NewRPCClient(rpcURL, rpcUser, rpcPass)
		// Test connection early to give clear feedback
		if err := rpcClient.TestConnection(); err != nil {
			return nil, fmt.Errorf("failed to connect to Dogecoin node: %v", err)
		}
		headerFetcher = NewHeaderFetcher(rpcClient)
	}

	miner := NewScryptMinerWithConfig(config.MaxHashes, config.ThreadCount)
	miner.DebugLogging = config.DebugLogging
	gpuMiner := NewGPUMiner(config.MaxHashes, config.ThreadCount)
	if config.NonceSearchMode == "" {
		config.NonceSearchMode = "sequential"
	}
	if config.NonceStartMode == "" {
		config.NonceStartMode = "calculated"
	}
	miner.SetNonceStrategy(config.NonceSearchMode, config.NonceStartMode, config.UseStride)
	gpuMiner.SetNonceStrategy(config.NonceSearchMode, config.NonceStartMode, config.UseStride)

	// Payment monitor uses only the P2P header stream (pure P2P, no RPC or external APIs).
	// In P2P mode: share the main client so the tipHistory it builds during sync is available.
	// In RPC mode: create a P2P client for the monitor so it can independently track headers.
	var paymentMonitor *P2PPaymentMonitor
	if p2pClient != nil {
		paymentMonitor = NewP2PPaymentMonitor(p2pClient, config.PayoutAddr, config.Network)
		go paymentMonitor.StartMonitoring(60 * time.Second)
	} else {
		// RPC mode: spin up a lightweight P2P client just for the payment monitor header stream.
		monitorP2P := NewP2PClient(config.Network)
		paymentMonitor = NewP2PPaymentMonitor(monitorP2P, config.PayoutAddr, config.Network)
		go monitorP2P.SyncHeadersForMonitor(config.Network) // background header sync for tip history
		go paymentMonitor.StartMonitoring(60 * time.Second)
	}

	m := &Miner{
		rpcClient:      rpcClient,
		p2pClient:      p2pClient,
		miner:          miner,
		gpuMiner:       gpuMiner,
		headerFetcher:  headerFetcher,
		paymentMonitor: paymentMonitor,
		config:         config,
		stats: &MiningStats{
			StartTime:     time.Now(),
			Status:        "stopped",
			LastUpdate:    time.Now(),
			LogMessages:   make([]string, 0, 100), // Pre-allocate for 100 messages
			PayoutAddress: config.PayoutAddr,
		},
		stopChan:          make(chan struct{}),
		clients:           make(map[*websocket.Conn]bool),
		enableHashLogging: false, // Disabled by default for high-speed runs (can enable via UI)
		extraNonce:        1,     // Start from 1 (skip zero) - sequential increment like real miners
		submittedBlocks:   make([]SubmittedBlock, 0),
		optimizer:         NewMinerOptimizer(nil), // Will be set to m after initialization
	}

	// Set optimizer reference to this miner
	m.optimizer.miner = m

	// Run diagnostic test on startup
	go func() {
		time.Sleep(1 * time.Second)
		m.runDiagnosticTest()
	}()

	return m, nil
}

// isDebugLogging safely reads the DebugLogging config flag with mutex protection
func (m *Miner) isDebugLogging() bool {
	m.configMutex.RLock()
	defer m.configMutex.RUnlock()
	return m.config.DebugLogging
}

// newMinerP2POnly creates a miner in P2P-only mode (no RPC client or connection test).
// Used at startup when saved config has ConnectionMode "p2p".
func newMinerP2POnly(config *MinerConfig) (*Miner, error) {
	if config == nil {
		config = &MinerConfig{ConnectionMode: "p2p"}
	}
	config.ConnectionMode = "p2p"
	// Always check for GPU on startup (override saved config if GPU now available)
	log.Printf("🔍 Checking for GPU availability...")
	if IsGPUAvailable() {
		config.DeviceType = "gpu"
		log.Printf("🎮 GPU detected - mining will use GPU (OpenCL accelerated)")
	} else {
		config.DeviceType = "cpu"
		log.Printf("💻 GPU not available - mining will use CPU")
	}
	if config.ThreadCount <= 0 {
		config.ThreadCount = 1
	}
	if config.MaxHashes == 0 {
		config.MaxHashes = 100000
	}
	if config.MiningIntensity == "" {
		config.MiningIntensity = "medium"
	}
	if config.NonceSearchMode == "" {
		config.NonceSearchMode = "sequential"
	}
	if config.NonceStartMode == "" {
		config.NonceStartMode = "calculated"
	}
	if config.Network == "" {
		config.Network = "mainnet"
	}
	p2pClient := NewP2PClient(config.Network)
	// Use network-specific peer override, falling back to legacy field
	peerOverride := config.P2PPeerOverride // legacy
	if config.Network == "testnet" && config.P2PPeerOverrideTestnet != "" {
		peerOverride = config.P2PPeerOverrideTestnet
	} else if config.Network == "mainnet" && config.P2PPeerOverrideMainnet != "" {
		peerOverride = config.P2PPeerOverrideMainnet
	}
	if peerOverride != "" {
		p2pClient.SetPeerOverride(peerOverride)
	}

	// Share the main P2P client with the payment monitor (read-only cached tip).
	paymentMonitor := NewP2PPaymentMonitor(p2pClient, config.PayoutAddr, config.Network)
	go paymentMonitor.StartMonitoring(60 * time.Second)

	mOut := &Miner{
		rpcClient:      nil,
		p2pClient:      p2pClient,
		headerFetcher:  nil,
		miner:          NewScryptMinerWithConfig(config.MaxHashes, config.ThreadCount),
		gpuMiner:       NewGPUMiner(config.MaxHashes, config.ThreadCount),
		paymentMonitor: paymentMonitor,
		config:         config,
		stats: &MiningStats{
			StartTime:     time.Now(),
			Status:        "stopped",
			LastUpdate:    time.Now(),
			LogMessages:   make([]string, 0, 100), // Pre-allocate for 100 messages
			PayoutAddress: config.PayoutAddr,
		},
		stopChan:          make(chan struct{}),
		clients:           make(map[*websocket.Conn]bool),
		enableHashLogging: false,
		extraNonce:        1,
		submittedBlocks:   make([]SubmittedBlock, 0),
		optimizer:         NewMinerOptimizer(nil), // Will be set to mOut after initialization
	}
	mOut.miner.DebugLogging = config.DebugLogging
	mOut.miner.SetNonceStrategy(config.NonceSearchMode, config.NonceStartMode, config.UseStride)
	mOut.gpuMiner.SetNonceStrategy(config.NonceSearchMode, config.NonceStartMode, config.UseStride)

	// Set optimizer reference to this miner
	mOut.optimizer.miner = mOut

	return mOut, nil
}

func (m *Miner) UpdateConfig(config *MinerConfig) error {
	// Normalize connection mode and network
	if config.ConnectionMode == "" {
		config.ConnectionMode = "rpc"
	}
	if config.Network == "" {
		config.Network = "mainnet"
	}
	// Use the payout address for the current network (miner uses PayoutAddr)
	if config.Network == "testnet" {
		config.PayoutAddr = config.PayoutAddrTestnet
	} else {
		config.PayoutAddr = config.PayoutAddrMainnet
	}

	// Test new connection first (RPC or P2P)
	var newRPCClient *RPCClient
	var newP2PClient *P2PClient
	switch config.ConnectionMode {
	case "p2p":
		newP2PClient = NewP2PClient(config.Network)
		// Use network-specific peer override, falling back to legacy field
		peerOverride := config.P2PPeerOverride // legacy
		if config.Network == "testnet" && config.P2PPeerOverrideTestnet != "" {
			peerOverride = config.P2PPeerOverrideTestnet
		} else if config.Network == "mainnet" && config.P2PPeerOverrideMainnet != "" {
			peerOverride = config.P2PPeerOverrideMainnet
		}
		if peerOverride != "" {
			newP2PClient.SetPeerOverride(peerOverride)
		}
	default:
		config.ConnectionMode = "rpc"
		newRPCClient = NewRPCClient(config.RPCURL, config.RPCUser, config.RPCPass)
		if err := newRPCClient.TestConnection(); err != nil {
			return fmt.Errorf("failed to connect with new settings: %v", err)
		}
	}

	// Stop current operations if running
	m.statsMutex.RLock()
	isRunning := m.stats.Status == "running"
	m.statsMutex.RUnlock()

	if isRunning {
		m.Stop()
	}

	// Set defaults for mining settings if not provided
	// IMPORTANT: Preserve current DeviceType if not explicitly changed by user
	if config.DeviceType == "" {
		// Use current device type, or default to cpu if no current config exists
		if m.config != nil {
			config.DeviceType = m.config.DeviceType
		} else {
			config.DeviceType = "cpu"
		}
	}
	if config.ThreadCount <= 0 {
		if config.DeviceType == "gpu" {
			config.ThreadCount = runtime.NumCPU() // Use all CPU cores for optimized parallel mining
		} else {
			config.ThreadCount = 1
		}
	}
	if config.ThreadCount > 64 {
		config.ThreadCount = 64 // Cap for high-core systems
	}
	if config.MaxHashes == 0 {
		config.MaxHashes = 100000
	}
	if config.MaxHashes > 1000000 {
		config.MaxHashes = 1000000 // Cap at 1M hashes
	}
	if config.MiningIntensity == "" {
		config.MiningIntensity = "medium"
	}
	// Set defaults for nonce search strategy
	if config.NonceSearchMode == "" {
		config.NonceSearchMode = "sequential"
	}
	if config.NonceStartMode == "" {
		config.NonceStartMode = "calculated"
	}

	// Apply mining intensity to max hashes if needed
	switch config.MiningIntensity {
	case "low":
		if config.MaxHashes > 50000 {
			config.MaxHashes = 50000
		}
	case "medium":
		if config.MaxHashes < 50000 {
			config.MaxHashes = 100000
		}
	case "high":
		if config.MaxHashes < 100000 {
			config.MaxHashes = 500000
		}
	case "extreme":
		if config.MaxHashes < 500000 {
			config.MaxHashes = 1000000
		}
	}

	// Restart payment monitor when network or payout address changes.
	needsNewMonitor := false
	m.configMutex.RLock()
	if m.config.Network != config.Network || m.config.PayoutAddr != config.PayoutAddr {
		needsNewMonitor = true
	}
	m.configMutex.RUnlock()

	if needsNewMonitor {
		if m.paymentMonitor != nil {
			m.paymentMonitor.Stop()
		}
		// Always use P2P header stream for payment verification (pure P2P, no RPC/external APIs).
		// P2P mode: share the new main client so its tipHistory is available immediately.
		// RPC mode: create a dedicated lightweight P2P client that syncs headers independently.
		var newMonitor *P2PPaymentMonitor
		if newP2PClient != nil {
			newMonitor = NewP2PPaymentMonitor(newP2PClient, config.PayoutAddr, config.Network)
		} else {
			monitorP2P := NewP2PClient(config.Network)
			newMonitor = NewP2PPaymentMonitor(monitorP2P, config.PayoutAddr, config.Network)
			go monitorP2P.SyncHeadersForMonitor(config.Network)
		}
		go newMonitor.StartMonitoring(60 * time.Second)
		m.paymentMonitor = newMonitor
		log.Printf("💰 Payment monitor restarted for %s (%s)", config.PayoutAddr, config.Network)
	}

	// Update configuration
	m.configMutex.Lock()
	m.config = config
	m.rpcClient = newRPCClient
	m.p2pClient = newP2PClient
	if newRPCClient != nil {
		m.headerFetcher = NewHeaderFetcher(newRPCClient)
	} else {
		m.headerFetcher = nil
	}
	// Update miner with new settings
	m.miner.MaxHashes = config.MaxHashes
	m.miner.ThreadCount = config.ThreadCount
	m.miner.DebugLogging = config.DebugLogging
	m.miner.SetNonceStrategy(config.NonceSearchMode, config.NonceStartMode, config.UseStride)
	// Update GPU miner
	m.gpuMiner = NewGPUMiner(config.MaxHashes, config.ThreadCount)
	m.gpuMiner.SetNonceStrategy(config.NonceSearchMode, config.NonceStartMode, config.UseStride)
	m.configMutex.Unlock()

	// Save configuration to file
	if err := saveConfigToFile(config); err != nil {
		log.Printf("Warning: Failed to save configuration to file: %v", err)
		// Don't fail the update if file save fails
	}

	log.Printf("Configuration updated successfully (Device: %s, Threads: %d, MaxHashes: %d, Intensity: %s)",
		config.DeviceType, config.ThreadCount, config.MaxHashes, config.MiningIntensity)
	return nil
}

func (m *Miner) GetConfig() *MinerConfig {
	m.configMutex.RLock()
	defer m.configMutex.RUnlock()

	config := *m.config
	return &config
}

// logHashAttempts logs hash attempts to console and broadcasts to web clients
func (m *Miner) logHashAttempts(hashesAttempted uint64, height int64, target string) {
	m.logMutex.RLock()
	shouldLog := m.enableHashLogging
	m.logMutex.RUnlock()

	if !shouldLog {
		return
	}

	var logMsg string
	// Rate limit logging - only log every 1000 hashes or significant milestones
	if hashesAttempted >= 1000 || hashesAttempted%1000 == 0 {
		logMsg = fmt.Sprintf("🔍 Mining Block #%d: Attempted %d hashes - Trying to guess winning hash...",
			height, hashesAttempted)
		log.Printf("%s (Target: %s)", logMsg, target)
	} else if hashesAttempted > 0 {
		// Log smaller attempts less frequently
		if hashesAttempted%100 == 0 {
			logMsg = fmt.Sprintf("🔍 Mining: %d hashes attempted on Block #%d", hashesAttempted, height)
			log.Print(logMsg)
		} else {
			return // Don't broadcast very frequent logs
		}
	}

	// Broadcast to web clients if we have a message
	if logMsg != "" {
		m.broadcastLogMessage(logMsg)
	}
}

// broadcastLogMessage sends a log message to all connected web clients
func (m *Miner) broadcastLogMessage(message string) {
	// Truncate very long messages to prevent memory issues
	const maxMessageLength = 500
	if len(message) > maxMessageLength {
		message = message[:maxMessageLength] + "... (truncated)"
	}

	// Add to stats log messages (keep last 100 for better history)
	m.statsMutex.Lock()
	if m.stats.LogMessages == nil {
		m.stats.LogMessages = make([]string, 0, 100)
	}
	m.stats.LogMessages = append(m.stats.LogMessages, message)
	// Keep only last 100 messages (aggressive cleanup)
	if len(m.stats.LogMessages) > 100 {
		// Remove oldest half to avoid frequent reallocation
		m.stats.LogMessages = m.stats.LogMessages[len(m.stats.LogMessages)-100:]
	}
	m.statsMutex.Unlock()

	// DON'T broadcast stats here - it's too frequent and kills hashrate!
	// The 2-second ticker will broadcast logs automatically
}

// SetHashLogging enables/disables hash attempt logging
func (m *Miner) SetHashLogging(enabled bool) {
	m.logMutex.Lock()
	defer m.logMutex.Unlock()
	m.enableHashLogging = enabled
}

// saveConfigToFile saves the configuration to a JSON file
func saveConfigToFile(config *MinerConfig) error {
	configPath := getConfigPath()

	// Create directory if it doesn't exist
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %v", err)
	}

	// Marshal config to JSON
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}

	// Write to file
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}

	log.Printf("Configuration saved to %s", configPath)
	return nil
}

// loadConfigFromFile loads the configuration from a JSON file
func loadConfigFromFile() (*MinerConfig, error) {
	configPath := getConfigPath()

	// Check if file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("config file does not exist")
	}

	// Read file
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	// Unmarshal JSON
	var config MinerConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}
	if config.Network == "" {
		config.Network = "mainnet"
	}
	// Migration: old config had only payoutAddr; use it for mainnet so we don't lose it
	if config.PayoutAddrMainnet == "" && config.PayoutAddr != "" {
		config.PayoutAddrMainnet = config.PayoutAddr
	}
	// Active payout is the one for current network
	if config.Network == "testnet" {
		config.PayoutAddr = config.PayoutAddrTestnet
	} else {
		config.PayoutAddr = config.PayoutAddrMainnet
	}

	log.Printf("Configuration loaded from %s", configPath)
	return &config, nil
}

// getConfigPath returns the path to the configuration file
func getConfigPath() string {
	// Try to use executable directory first
	if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		return filepath.Join(exeDir, configFileName)
	}
	// Fallback to current directory
	return configFileName
}

// getTipPath returns the path for the saved chain tip (same directory as config).
// Uses different file for testnet so mainnet and testnet tips don't mix.
func getTipPath(network string) string {
	name := chainTipFileName
	if network == "testnet" {
		name = "chain_tip_testnet.json"
	}
	return filepath.Join(filepath.Dir(getConfigPath()), name)
}

// savedChainTip is the JSON shape of chain_tip.json (P2P resume)
type savedChainTip struct {
	Height         int64  `json:"height"`
	HashHex        string `json:"hash"`
	LastNonMinBits uint32 `json:"lastNonMinBits,omitempty"` // testnet: last nBits seen != powLimit; avoids bad-diffbits after resume
}

// loadSavedTip loads the last known block tip from disk. Returns height, hash (hex), lastNonMinBits (0 if not set), true if found and valid.
func loadSavedTip(network string) (height int64, hashHex string, lastNonMinBits uint32, ok bool) {
	if network == "" {
		network = "mainnet"
	}
	data, err := os.ReadFile(getTipPath(network))
	if err != nil {
		return 0, "", 0, false
	}
	var tip savedChainTip
	if err := json.Unmarshal(data, &tip); err != nil {
		return 0, "", 0, false
	}
	if tip.Height < 0 || len(tip.HashHex) != 64 {
		return 0, "", 0, false
	}
	return tip.Height, tip.HashHex, tip.LastNonMinBits, true
}

// clearSavedTip removes the saved chain tip so the next sync starts from genesis (e.g. after invalid/corrupt tip).
func clearSavedTip(network string) {
	if network == "" {
		network = "mainnet"
	}
	path := getTipPath(network)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("Failed to clear saved tip file: %v", err)
		return
	}
	log.Printf("Cleared saved chain tip; will sync from genesis on next attempt")
}

// Throttle "Saved chain tip" log to at most every 30s to avoid spam on fast chains (testnet)
var lastSavedTipLogTime time.Time
var lastSavedTipLogHeight int64 = -1

// saveTip persists the current chain tip (and optional lastNonMinBits for testnet) so the miner can resume on next start.
// Only logs when the tip actually changed (avoids repeating the same message every poll).
func saveTip(height int64, hashHex string, network string, lastNonMinBits uint32) {
	if network == "" {
		network = "mainnet"
	}
	prevHeight, prevHash, _, hadPrev := loadSavedTip(network)
	if hadPrev && prevHeight == height && prevHash == hashHex {
		return // unchanged, skip write and log
	}
	tip := savedChainTip{Height: height, HashHex: hashHex, LastNonMinBits: lastNonMinBits}
	data, err := json.MarshalIndent(tip, "", "  ")
	if err != nil {
		return
	}
	path := getTipPath(network)
	dir := filepath.Dir(path)
	_ = os.MkdirAll(dir, 0755)
	if err := os.WriteFile(path, data, 0600); err != nil {
		log.Printf("Failed to save chain tip: %v", err)
		return
	}
	// Log at most every 30s to avoid spam on testnet
	if height != lastSavedTipLogHeight && (lastSavedTipLogHeight == -1 || time.Since(lastSavedTipLogTime) > 30*time.Second) {
		log.Printf("Saved chain tip for resume: height %d", height)
		lastSavedTipLogHeight = height
		lastSavedTipLogTime = time.Now()
	}
}

func (m *Miner) Start() error {
	m.configMutex.RLock()
	config := m.config
	m.configMutex.RUnlock()

	// Validate miner is initialized
	if config == nil {
		return errors.New("miner configuration not initialized")
	}

	// Payout address is required for mainnet and testnet
	payout := config.PayoutAddr
	if config.Network == "testnet" && config.PayoutAddrTestnet != "" {
		payout = config.PayoutAddrTestnet
	}
	if config.Network == "mainnet" && config.PayoutAddrMainnet != "" {
		payout = config.PayoutAddrMainnet
	}
	if strings.TrimSpace(payout) == "" {
		return errors.New("⚠️ Payout address is required. Please set it in Configuration before starting mining.")
	}

	// Only create/use RPC and header fetcher when connection mode is RPC
	if config.ConnectionMode != "p2p" {
		if m.rpcClient == nil {
			if config.RPCURL != "" && config.RPCUser != "" && config.RPCPass != "" {
				rpcClient := NewRPCClient(config.RPCURL, config.RPCUser, config.RPCPass)
				if err := rpcClient.TestConnection(); err != nil {
					return fmt.Errorf("RPC not configured or connection failed: %v", err)
				}
				m.configMutex.Lock()
				m.rpcClient = rpcClient
				m.headerFetcher = NewHeaderFetcher(rpcClient)
				m.configMutex.Unlock()
			} else {
				return fmt.Errorf("RPC not configured. Please configure RPC settings first")
			}
		}
	} else {
		// P2P mode: ensure we don't use RPC or header fetcher
		m.configMutex.Lock()
		m.headerFetcher = nil
		m.configMutex.Unlock()
	}

	// Ensure stopChan exists and is open
	if m.stopChan == nil {
		m.stopChan = make(chan struct{})
	} else {
		// Check if stopChan is already closed
		select {
		case <-m.stopChan:
			// Channel was closed, recreate it
			m.stopChan = make(chan struct{})
		default:
			// Channel is open, good to go
		}
	}

	m.statsMutex.Lock()
	m.stats.Status = "running"
	m.statsMutex.Unlock()

	// Broadcast mining started message
	m.broadcastLogMessage("🚀 Mining started - Ready to guess winning hashes!")

	// Start header fetcher for autonomous operation (only tracks current block, doesn't fetch history)
	if m.headerFetcher != nil {
		m.headerFetcher.Start()
	}

	// Start auto-optimizer for Lucky mining (monitors and adjusts parameters every 2 minutes)
	if m.optimizer != nil {
		m.optimizer.Start()
		log.Printf("⚙️ AUTO-OPTIMIZER: Started - will monitor and adjust mining parameters dynamically")
	} else {
		log.Printf("⚠️ AUTO-OPTIMIZER: Not available")
	}
	m.broadcastLogMessage("✨ GPU/CPU Auto-detection enabled - mining with optimal device")

	m.wg.Add(1)
	go m.miningLoop()

	log.Println("Miner started")
	m.broadcastLogMessage("✅ Miner initialized and running")
	return nil
}

func (m *Miner) Stop() {
	// Disconnect P2P so any readMessage() in the mining loop unblocks and the loop can exit.
	// Otherwise two goroutines (old + new after restart) would read from the same conn and cause magic mismatch.
	if m.p2pClient != nil {
		m.p2pClient.Disconnect()
	}

	// Signal stop by closing the channel
	if m.stopChan != nil {
		select {
		case <-m.stopChan:
			// Already closed, nothing to do
		default:
			// Close it
			close(m.stopChan)
		}
	}

	// Stop header fetcher
	if m.headerFetcher != nil {
		m.headerFetcher.Stop()
	}

	// Stop auto-optimizer
	if m.optimizer != nil {
		m.optimizer.Stop()
	}

	// Wait for mining loop to finish
	m.wg.Wait()

	// Recreate stopChan for potential restart
	m.stopChan = make(chan struct{})

	m.statsMutex.Lock()
	m.stats.Status = "stopped"
	m.statsMutex.Unlock()

	log.Println("Miner stopped")
	m.broadcastLogMessage("⏹️ Mining stopped")
}

func (m *Miner) miningLoop() {
	defer m.wg.Done()

	// Add panic recovery to prevent total crash
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ PANIC in mining loop (recovered): %v", r)
			log.Printf("Mining loop crashed but application continues. You can restart mining.")
			m.statsMutex.Lock()
			m.stats.Status = "error"
			m.statsMutex.Unlock()
			m.broadcastLogMessage(fmt.Sprintf("❌ Mining crashed: %v - Restart mining to continue", r))
		}
	}()

	// Push stats to web UI every 2s so hashrate, est. time to block, time on block update even when blocked in a long MineBlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("⚠️ Panic in stats broadcaster (recovered): %v", r)
			}
		}()
		statsTicker := time.NewTicker(2 * time.Second)
		defer statsTicker.Stop()
		for {
			select {
			case <-m.stopChan:
				return
			case <-statsTicker.C:
				m.broadcastStats()
			}
		}
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	hashCounter := uint64(0)
	lastHashTime := time.Now()

	// Track last logged block to avoid repetitive logging
	lastLoggedBlockHeight := int64(-1)
	lastLoggedPayoutBlock := int64(-1)
	// Throttle noisy logs on testnet (rounds every ~0.3s)
	lastCheckpointLog := time.Time{}
	lastRoundDoneLog := time.Time{}
	roundDoneCount := 0
	// P2P initialization state
	p2pInitialized := false
	// Adaptive testnet round cap: tunes hashes/round based on observed tip movement and stall behavior.
	// Start conservative so we can follow very fast testnet bursts immediately.
	adaptiveTestnetRoundCap := uint64(12000)
	lastAdaptiveLog := time.Time{}
	stableTestnetRounds := 0
	lastCapAdjust := time.Time{}

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			// Update hash rate from recent hashes (only when we have new data - don't overwrite with 0)
			elapsed := time.Since(lastHashTime).Seconds()
			if elapsed > 0 {
				m.statsMutex.Lock()
				if hashCounter > 0 {
					m.stats.HashRate = float64(hashCounter) / elapsed
				}
				// Always update LastUpdate so UI knows we're alive
				m.stats.LastUpdate = time.Now()
				m.statsMutex.Unlock()
				hashCounter = 0
				lastHashTime = time.Now()
			}

			// DON'T broadcast here - the dedicated 2-second statsTicker handles it
		case <-time.After(100 * time.Millisecond):
			// Snapshot config and clients
			m.configMutex.RLock()
			mode := m.config.ConnectionMode
			network := m.config.Network
			rpcClient := m.rpcClient
			p2pClient := m.p2pClient
			payoutAddr := m.config.PayoutAddr
			m.configMutex.RUnlock()

			var template *BlockTemplate
			var err error

			switch mode {
			case "p2p":
				// Pure P2P mode: get tip header from peers, build template locally
				if p2pClient == nil {
					// Lazy-init P2P client if needed
					m.configMutex.Lock()
					if m.p2pClient == nil {
						net := m.config.Network
						if net == "" {
							net = "mainnet"
						}
						m.p2pClient = NewP2PClient(net)
					}
					p2pClient = m.p2pClient
					m.configMutex.Unlock()
					p2pInitialized = false // Reset flag when client reinitializes
				}

				if p2pClient == nil {
					log.Printf("P2P client not available, retrying...")
					time.Sleep(5 * time.Second)
					continue
				}

				// Log current cached tip before fetching template
				if m.isDebugLogging() {
					if currentCachedTip := p2pClient.GetCachedTip(); currentCachedTip != nil {
						log.Printf("[TEMPLATE] Fetching template from cached tip at height %d", currentCachedTip.Height)
					}
				}

				// One-time initialization when P2P client is first created
				if !p2pInitialized {
					// Apply P2P log callback and optional peer override; start from checkpoint (if set) or saved tip or genesis
					p2pClient.SetLogFunc(m.broadcastLogMessage)
					m.configMutex.RLock()
					checkpointID := m.config.P2PCheckpoint
					network := m.config.Network
					// Use network-specific peer override, falling back to legacy field
					peerOverride := m.config.P2PPeerOverride // legacy
					if network == "testnet" && m.config.P2PPeerOverrideTestnet != "" {
						peerOverride = m.config.P2PPeerOverrideTestnet
					} else if network == "mainnet" && m.config.P2PPeerOverrideMainnet != "" {
						peerOverride = m.config.P2PPeerOverrideMainnet
					}
					m.configMutex.RUnlock()
					if peerOverride != "" {
						p2pClient.SetPeerOverride(peerOverride)
					}

					// Priority: Force genesis (after validation failure) > Explicit checkpoint > Saved tip > Genesis
					if p2pForceGenesisNext {
						p2pForceGenesisNext = false
						clearSavedTip(m.config.Network)
						_ = p2pClient.SetStartCheckpoint("", 0)
						log.Printf("📍 Syncing from genesis (recovery after chain validation failure; may take several minutes)")
						lastCheckpointLog = time.Now()
					} else if checkpointID != "" {
						// User selected a checkpoint (including "0" for genesis)
						_ = p2pClient.SetStartCheckpointByID(checkpointID)
						if time.Since(lastCheckpointLog) > 60*time.Second {
							log.Printf("📍 Starting sync from configured checkpoint: %s", checkpointID)
							lastCheckpointLog = time.Now()
						}
					} else if savedHeight, savedHash, savedLastNonMin, ok := loadSavedTip(m.config.Network); ok {
						// Prefer built-in checkpoint hash when saved height matches (avoids stale/wrong saved hash)
						if m.config.Network == "testnet" && savedHeight == 38150909 {
							_ = p2pClient.SetStartCheckpointByID("38150909")
						} else if m.config.Network == "testnet" && savedHeight == 42799352 {
							_ = p2pClient.SetStartCheckpointByID("42799352")
						} else if savedHeight == 0 {
							_ = p2pClient.SetStartCheckpointByID("0")
						} else {
							_ = p2pClient.SetStartCheckpoint(savedHash, savedHeight)
						}
						if savedLastNonMin != 0 {
							p2pClient.SeedLastNonMinDifficultyBits(savedLastNonMin)
						}
						if time.Since(lastCheckpointLog) > 60*time.Second {
							log.Printf("📍 Resuming from saved tip: height %d", savedHeight)
							lastCheckpointLog = time.Now()
						}
					} else {
						if time.Since(lastCheckpointLog) > 60*time.Second {
							log.Printf("📍 Starting sync from genesis (no checkpoint or saved tip)")
							lastCheckpointLog = time.Now()
						}
					}

					// Do initial sync with retries (result used to validate chain, then background sync takes over)
					var errTip error
					const tipRetries = 3
					backoffSecs := []int{5, 15, 30} // exponential backoff between retries (longer to avoid rate limiting)
					for attempt := 0; attempt < tipRetries; attempt++ {
						_, errTip = p2pClient.GetTipHeader()
						if errTip == nil {
							break
						}
						// Connection drop: ensure we'll try a different peer next time, then retry after backoff
						connectionLost := errors.Is(errTip, io.EOF) ||
							strings.Contains(errTip.Error(), "connection reset") ||
							strings.Contains(errTip.Error(), "connection refused") ||
							strings.Contains(errTip.Error(), "forcibly closed")
						if connectionLost {
							p2pClient.Disconnect()
							if attempt < tipRetries-1 {
								delay := 2
								if attempt < len(backoffSecs) {
									delay = backoffSecs[attempt]
								}
								log.Printf("P2P: connection lost (%v), reconnecting in %ds...", errTip, delay)
								time.Sleep(time.Duration(delay) * time.Second)
								continue
							}
							log.Printf("P2P: could not get tip after %d attempts: %v", tipRetries, errTip)
							if m.config.Network == "testnet" {
								log.Printf("   Testnet has only one DNS seed. If it keeps disconnecting, set Configuration → P2P peer override to a testnet node (e.g. host:44556).")
							}
						} else {
							log.Printf("Error getting P2P tip header: %v", errTip)
						}
						// If chain didn't link (reorg or invalid tip), retry from user's chosen checkpoint (or genesis if none chosen)
						if strings.Contains(errTip.Error(), "non-continuous headers") || strings.Contains(errTip.Error(), "does not link") {
							peerAddr := p2pClient.GetPeerAddr()
							p2pClient.Disconnect()
							if peerAddr != "" {
								p2pClient.MarkPeerBad(peerAddr)
							}
							p2pClient.ClearSyncState()
							clearSavedTip(m.config.Network)
							m.configMutex.RLock()
							userCheckpoint := m.config.P2PCheckpoint
							m.configMutex.RUnlock()
							if userCheckpoint != "" && userCheckpoint != "0" {
								// User chose a checkpoint — always retry from that, never force genesis
								_ = p2pClient.SetStartCheckpointByID(userCheckpoint)
								log.Printf("⚠️ Chain validation failed - retrying from your chosen checkpoint: %s (will try a different peer if available)", userCheckpoint)
							} else {
								// No checkpoint or genesis chosen — next sync will use genesis
								_ = p2pClient.SetStartCheckpoint("", 0)
								p2pForceGenesisNext = true
								log.Printf("⚠️ Chain validation failed - next sync will use genesis (may take several minutes)")
							}
						}
						break
					}
					if errTip != nil {
						// Truncated AuxPoW or EOF from peer — disconnect so next attempt may use a different peer or get a complete message
						if strings.Contains(errTip.Error(), "skip auxpow") || strings.Contains(errTip.Error(), "EOF") {
							p2pClient.Disconnect()
							log.Printf("P2P: disconnected peer after truncated/EOF response; will retry")
						}
						time.Sleep(10 * time.Second)
						continue
					}

					// Start background header sync NOW (after initial headers are loaded)
					p2pClient.StartBackgroundHeaderSync()
					p2pInitialized = true
					log.Printf("✅ P2P initialized and mining — headers updated every 300ms on testnet")
				}

				// Testnet can run far faster than mainnet (min-diff abuse until a network rule change; see
				// https://github.com/dogecoin/dogecoin/pull/3967 ). Poll headers before each template so we
				// usually build on the latest tip. Mainnet stays lightly throttled.
				if network == "testnet" {
					p2pClient.RefreshHeadersIfStale(1 * time.Nanosecond)
				} else {
					p2pClient.RefreshHeadersIfStale(4 * time.Second)
				}

				// Use cached tip from background sync (non-blocking)
				tipHeader := p2pClient.GetCachedTip()
				if tipHeader == nil {
					// Should not happen if initialization succeeded, but handle gracefully
					log.Printf("⚠️ No cached header from P2P yet, waiting...")
					time.Sleep(1 * time.Second)
					continue
				}

				// Save tip in display order so P2P getheaders locator is correct on resume
				hashDisplay := make([]byte, 32)
				copy(hashDisplay, tipHeader.Hash[:])
				reverseBytesInPlace(hashDisplay)
				saveTip(tipHeader.Height, hex.EncodeToString(hashDisplay), m.config.Network, p2pClient.GetLastNonMinDifficultyBits())

				lastNonMinBits := p2pClient.GetLastNonMinDifficultyBits()
				var lastNonMinPtr *uint32
				if lastNonMinBits != 0 {
					lastNonMinPtr = &lastNonMinBits
				}
				tipParentTS := p2pClient.GetTipParentTimestamp()
				template, err = BuildTemplateFromHeader(tipHeader, payoutAddr, m.config.Network, lastNonMinPtr, tipParentTS)
				if err != nil {
					log.Printf("Error building template from P2P header: %v", err)
					time.Sleep(5 * time.Second)
					continue
				}

			default: // "rpc" (or unknown -> treated as rpc)
				if rpcClient == nil {
					// RPC client not available, wait a bit before retrying
					time.Sleep(5 * time.Second)
					continue
				}

				// Build template in-app from chain tip (minimal RPC: no getblocktemplate needed)
				if _, tipHeader, errTip := rpcClient.GetTipHeader(); errTip == nil && tipHeader != nil {
					template, err = BuildTemplateFromHeader(tipHeader, payoutAddr, m.config.Network, nil, 0)
				}
				// Fallback to full getblocktemplate if tip-based build failed
				if template == nil || err != nil {
					template, err = rpcClient.GetBlockTemplate()
					if err != nil {
						log.Printf("Error getting block template: %v", err)
						time.Sleep(10 * time.Second)
						continue
					}
				}
			}

			// Create empty block (ONLY coinbase transaction - no mempool transactions)
			// This ensures we don't waste time processing transactions
			// Note: Any transactions in template.Transactions are explicitly ignored
			// Only log payout/coinbase at most once per 5 blocks to reduce spam on testnet

			// Align template with cached tip before mining (testnet: skip stale work)
			if mode == "p2p" && p2pClient != nil {
				cachedTip := p2pClient.GetCachedTip()
				if cachedTip != nil {
					prevOK := tipHashDisplayHex(cachedTip) == template.PreviousBlockHash
					heightOK := template.Height == cachedTip.Height+1
					if network == "testnet" && (!heightOK || !prevOK) {
						log.Printf("⏭️ Skipping stale template: mine height %d (tip=%d, prevMatch=%v) — refreshing",
							template.Height, cachedTip.Height, prevOK)
						p2pClient.RefreshHeadersIfStale(1 * time.Nanosecond)
						continue
					}
					if !heightOK {
						log.Printf("⚠️ LAG DETECTED: Cached tip height=%d, BUT mining block height=%d (should be %d). Template may be stale!", cachedTip.Height, template.Height, cachedTip.Height+1)
					} else {
						lastLoggedBlockHeight = template.Height
					}
				}
			}

			logPayoutInfo := (lastLoggedPayoutBlock != template.Height) && (lastLoggedPayoutBlock == -1 || template.Height-lastLoggedPayoutBlock >= 5)
			block, err := m.createEmptyBlock(template, logPayoutInfo)
			if err != nil {
				log.Printf("Error creating block: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}
			if logPayoutInfo {
				lastLoggedPayoutBlock = template.Height
			}

			// Update block timestamp if needed (reuse block struct)
			block.Timestamp = uint32(template.CurTime)

			// Update current block info
			m.statsMutex.Lock()
			if m.stats.CurrentBlock == nil {
				m.stats.CurrentBlock = &BlockInfo{}
			}
			// Only reset if block HEIGHT actually changed
			blockChanged := false
			if m.stats.CurrentBlock.Height != template.Height {
				m.stats.CurrentBlock.HashesAttempted = 0 // Reset hash count only when block height changes
				blockChanged = true
				log.Printf("[RESET] Block height changed: %d → %d, HashesAttempted reset to 0", m.stats.CurrentBlock.Height, template.Height)
			}
			m.stats.CurrentBlock.Height = template.Height
			// Get difficulty - try from template first, fallback to RPC call in RPC mode
			if template.Difficulty > 0 {
				m.stats.CurrentBlock.Difficulty = template.Difficulty
			} else if mode == "rpc" && rpcClient != nil {
				// Fallback: get difficulty from RPC if template doesn't have it
				if diff, err := rpcClient.GetDifficulty(); err == nil {
					m.stats.CurrentBlock.Difficulty = diff
				}
			}
			m.stats.CurrentBlock.Target = template.Target
			m.statsMutex.Unlock()

			// Broadcast stats immediately (synchronously) when block changes so UI definitely sees reset to 0
			if blockChanged {
				m.broadcastStats()
			}

			// Mine the block with predictive/random strategy
			// This is REAL mining - trying to guess the winning hash by testing different nonces
			// We're actually computing Scrypt hashes and checking if they meet the target

			// Get device type from config
			m.configMutex.RLock()
			deviceType := m.config.DeviceType
			m.configMutex.RUnlock()

			var nonce uint32
			var hash []byte
			var found bool
			var hashesAttempted uint64

			// Get thread count for logging
			m.configMutex.RLock()
			threadCount := m.config.ThreadCount
			m.configMutex.RUnlock()

			// Pre-compute targetBig once per block (reuse for validation)
			targetBytes, err := hex.DecodeString(template.Target)
			var targetBig *big.Int
			if err == nil {
				targetBig = new(big.Int).SetBytes(targetBytes)
				if targetBig.Cmp(big.NewInt(0)) == 0 {
					targetBig = big.NewInt(1)
				}
			}
			// On testnet P2P: keep rounds SHORT so we fetch a fresh template every ~0.5s.
			// Testnet blocks arrive ~every 1s. A 4s round means we're 4 blocks stale before we submit.
			// At ~130 KH/s GPU, 65000 hashes = ~0.5s. This keeps us within 1 block of tip almost always.
			if mode == "p2p" && network == "testnet" {
				roundCap := adaptiveTestnetRoundCap
				m.miner.MaxHashesThisRound = roundCap
				m.gpuMiner.MaxHashesThisRound = roundCap
				if time.Since(lastCheckpointLog) > 60*time.Second || lastCheckpointLog.IsZero() {
					log.Printf("🔄 Testnet: %d hashes/round (dynamic) — tuning to tip speed", roundCap)
				}
			} else if mode == "p2p" && network == "mainnet" {
				// Mainnet ~1 min blocks: cap rounds so we re-check tip often (fewer orphans / stale parents).
				const mainnetHashesPerRound = 350000
				m.miner.MaxHashesThisRound = mainnetHashesPerRound
				m.gpuMiner.MaxHashesThisRound = mainnetHashesPerRound
			}
			blockAttemptStart := time.Now()
			if deviceType == "gpu" {
				// Only log start message once per block to reduce spam
				if lastLoggedBlockHeight != template.Height {
					if mode == "p2p" && network == "testnet" {
						log.Printf("⚡ Mining Block #%d - GPU (difficulty: %.2f, target: %s...)",
							template.Height, template.Difficulty, template.Target[:16])
					} else {
						log.Printf("⚡ Mining Block #%d - GPU", template.Height)
					}
					lastLoggedBlockHeight = template.Height
				}
				nonce, hash, found, hashesAttempted = m.gpuMiner.MineBlock(block, template.Target)
				if !found && hashesAttempted > 0 {
					roundDoneCount++
					elapsed := time.Since(blockAttemptStart).Seconds()
					if time.Since(lastRoundDoneLog) > 30*time.Second || roundDoneCount%20 == 1 {
						log.Printf("📊 Round done: %d hashes in %.1fs, fetching new template...", hashesAttempted, elapsed)
						lastRoundDoneLog = time.Now()
					}
				}
			} else {
				// Only log once per block
				if lastLoggedBlockHeight != template.Height {
					if threadCount > 1 {
						log.Printf("⛏️ CPU Mining Block #%d - %d threads", template.Height, threadCount)
					} else {
						log.Printf("⛏️ CPU Mining Block #%d", template.Height)
					}
					lastLoggedBlockHeight = template.Height
				}
				if m.isDebugLogging() {
					log.Printf("[MINING] Starting MineBlock for height %d with target %s (ThreadCount=%d, MaxHashes=%d)", template.Height, template.Target, m.miner.ThreadCount, m.miner.MaxHashes)
				}
				nonce, hash, found, hashesAttempted = m.miner.MineBlock(block, template.Target)
				if m.isDebugLogging() {
					log.Printf("[MINING] Completed MineBlock: found=%v, hashes=%d", found, hashesAttempted)
				}
				// Log CPU mining progress
				if !found && hashesAttempted > 0 {
					roundDoneCount++
					elapsed := time.Since(blockAttemptStart).Seconds()
					if time.Since(lastRoundDoneLog) > 10*time.Second || roundDoneCount%10 == 1 {
						log.Printf("📊 CPU: %d hashes in %.1fs, continuing...", hashesAttempted, elapsed)
						lastRoundDoneLog = time.Now()
					}
				}
			}
			if mode == "p2p" && network == "testnet" && p2pClient != nil {
				// Dynamic testnet tuning:
				// - If tip moved a lot during a round, lower cap (refresh template faster).
				// - If we repeatedly stall near cap, lower cap.
				// - If stable and finishing fast, raise cap gradually.
				prevCap := adaptiveTestnetRoundCap
				roundElapsed := time.Since(blockAttemptStart)
				cachedTipAfter := p2pClient.GetCachedTip()
				tipAdvance := int64(0)
				if cachedTipAfter != nil {
					tipAdvance = cachedTipAfter.Height - (template.Height - 1)
					if tipAdvance < 0 {
						tipAdvance = 0
					}
				}
				nearCap := hashesAttempted >= (prevCap*95)/100
				stalledNearCap := nearCap && hashesAttempted < prevCap

				// Avoid cap oscillation: adjust at most once per second.
				if lastCapAdjust.IsZero() || time.Since(lastCapAdjust) >= 1*time.Second {
					headersAge := p2pClient.HeadersAppliedAge()
					switch {
					case tipAdvance >= 4:
						adaptiveTestnetRoundCap = adaptiveTestnetRoundCap * 65 / 100
						stableTestnetRounds = 0
					case tipAdvance >= 1:
						adaptiveTestnetRoundCap = adaptiveTestnetRoundCap * 80 / 100
						stableTestnetRounds = 0
					case stalledNearCap && roundElapsed >= 1500*time.Millisecond:
						adaptiveTestnetRoundCap = adaptiveTestnetRoundCap * 90 / 100
						stableTestnetRounds = 0
					case headersAge > 1200*time.Millisecond && tipAdvance == 0:
						// Chain appears briefly stable (or header stream behind): hash more per round.
						adaptiveTestnetRoundCap = adaptiveTestnetRoundCap * 130 / 100
						stableTestnetRounds = 0
					case tipAdvance == 0 && hashesAttempted >= prevCap && roundElapsed <= 500*time.Millisecond:
						stableTestnetRounds++
						if stableTestnetRounds >= 3 {
							adaptiveTestnetRoundCap = adaptiveTestnetRoundCap * 115 / 100
							stableTestnetRounds = 0
						}
					default:
						stableTestnetRounds = 0
					}
					lastCapAdjust = time.Now()
				}

				if adaptiveTestnetRoundCap < 12000 {
					adaptiveTestnetRoundCap = 12000
				}
				if adaptiveTestnetRoundCap > 40000 {
					adaptiveTestnetRoundCap = 40000
				}

				if adaptiveTestnetRoundCap != prevCap && (lastAdaptiveLog.IsZero() || time.Since(lastAdaptiveLog) > 3*time.Second) {
					log.Printf("⚙️ Testnet dynamic cap: %d → %d (tipAdvance=%d, hashes=%d, round=%.2fs)",
						prevCap, adaptiveTestnetRoundCap, tipAdvance, hashesAttempted, roundElapsed.Seconds())
					lastAdaptiveLog = time.Now()
				}
			}
			if mode == "p2p" && (network == "testnet" || network == "mainnet") {
				m.miner.MaxHashesThisRound = 0
				m.gpuMiner.MaxHashesThisRound = 0
			}
			if m.isDebugLogging() {
			log.Printf("[MINING] Updating stats: hashesAttempted=%d, found=%v", hashesAttempted, found)
		}
			// Record time spent on this block attempt for UI
			m.statsMutex.Lock()
			m.stats.BlockAttemptSeconds = time.Since(blockAttemptStart).Seconds()
			m.statsMutex.Unlock()

			// Log hash attempts (if enabled) - but reduce frequency to avoid spam
			// Only log on significant milestones to avoid fmt.Sprintf overhead
			if hashesAttempted > 0 {
				m.logMutex.RLock()
				shouldLog := m.enableHashLogging
				m.logMutex.RUnlock()

				// Only log every 100K hashes or on block found (minimize fmt.Sprintf calls)
				if shouldLog && (hashesAttempted >= 100000 || hashCounter%100 == 0) {
					m.logHashAttempts(hashesAttempted, template.Height, template.Target)
				}
			}

			// Update hash counter with actual number of hashes attempted
			hashCounter += hashesAttempted
			// Update stats immediately so web UI shows hashrate/total (ticker broadcasts every 2s)
			if hashesAttempted > 0 && m.stats.BlockAttemptSeconds > 0 {
				m.statsMutex.Lock()
				m.stats.TotalHashes += hashesAttempted
				if m.stats.CurrentBlock != nil {
					m.stats.CurrentBlock.HashesAttempted += hashesAttempted // Update hashes for current block
				}
				m.stats.HashRate = float64(hashesAttempted) / m.stats.BlockAttemptSeconds
				m.stats.LastUpdate = time.Now()
				m.statsMutex.Unlock()
				hashCounter = 0 // already applied to TotalHashes above; avoid double-count when ticker runs
				// DON'T broadcast here - it's called multiple times per second and kills performance!
				// The 2-second ticker will broadcast automatically
			}

			// Validate block found result - must have valid nonce and hash (use same LE comparison as miners)
			if found && len(hash) > 0 {
				if targetBig != nil {
					targetBigBytes := targetBig.Bytes()
					targetBytesLE := make([]byte, 32)
					copy(targetBytesLE[32-len(targetBigBytes):], targetBigBytes)
					reverseBytesInPlace(targetBytesLE)
					if !hashMeetsTargetLE(hash, targetBytesLE) {
						log.Printf("⚠️ False positive detected - hash doesn't meet target, ignoring")
						found = false
					}
				}
			} else if found {
				// Invalid result (empty hash or zero nonce)
				log.Printf("⚠️ Invalid block result (nonce=%d, hashLen=%d), ignoring", nonce, len(hash))
				found = false
			}

			// Testnet P2P: do not broadcast a solve that no longer extends our best cached tip (almost always orphaned).
			if found && mode == "p2p" && network == "testnet" && p2pClient != nil {
				if ct := p2pClient.GetCachedTip(); ct != nil && tipHashDisplayHex(ct) != template.PreviousBlockHash {
					ahead := ct.Height - (template.Height - 1)
					if ahead < 0 {
						ahead = 0
					}
					log.Printf("⏭️ Discarding solve (PoW ok): parent no longer tip (candidate builds on h=%d, tip now h=%d, ~%d ahead). Not broadcasting.",
						template.Height-1, ct.Height, ahead)
					m.broadcastLogMessage(fmt.Sprintf("⏭️ Discarded orphan-risk block at h=%d — remining tip", template.Height))
					p2pClient.RefreshHeadersIfStale(1 * time.Nanosecond)
					found = false
				}
			}

			if found {
				// Only log important events (block found); hash in display order (matches block explorers)
				log.Printf("🎉 BLOCK FOUND! Hash: %s, Nonce: %d (after %d hashes)", HashToDisplayHex(hash), nonce, hashesAttempted)
				m.broadcastLogMessage("🎉 BLOCK FOUND! Check logs for details.")

				// Log the exact 80-byte header that was verified by GPU/CPU mining
				if m.isDebugLogging() {
					blockCopyForDebug := *block
					blockCopyForDebug.Nonce = nonce
					headerAtMining := blockCopyForDebug.SerializeHeader()
					logHeaderDebug("[MINING]", headerAtMining)
				}

				// Stale-tip detection: use the cached tip height we already know from the last GetTipHeader() call.
				// Do NOT call GetTipHeader() again here — BroadcastBlock() is about to read from the same
				// P2P connection and a concurrent GetTipHeader() would corrupt the message stream (magic mismatch).
				isStale := false
				blocksAhead := int64(0)
				if mode == "p2p" && p2pClient != nil {
					cachedTip := p2pClient.GetCachedTip()
					if cachedTip != nil {
						expectedPrevDisplay := make([]byte, 32)
						copy(expectedPrevDisplay, cachedTip.Hash[:])
						reverseBytesInPlace(expectedPrevDisplay)
						expectedPrevHex := hex.EncodeToString(expectedPrevDisplay)

						if expectedPrevHex != template.PreviousBlockHash {
							isStale = true
							blocksAhead = cachedTip.Height - (template.Height - 1)
							// Mainnet / RPC-style race: still try submit. Testnet discards above before we get here.
							log.Printf("⚠️ Tip moved while mining (height %d → %d, %d blocks ahead) — submitting block anyway", template.Height-1, cachedTip.Height, blocksAhead)
							log.Printf("   Block builds on: %s", template.PreviousBlockHash[:16]+"...")
							log.Printf("   Current tip is:  %s", expectedPrevHex[:16]+"...")
							log.Printf("   💡 When the network is many blocks ahead, your block is usually orphaned (no reward). You only get paid when your block extends the best chain.")
							m.broadcastLogMessage(fmt.Sprintf("⚠️ Tip moved (%d blocks ahead) — submitting anyway (often orphaned = no pay)", blocksAhead))
						}
					}
				}

				// Track block submission
				submittedBlock := SubmittedBlock{
					Height:     template.Height,
					Hash:       HashToDisplayHex(hash),
					Nonce:      nonce,
					SubmitTime: time.Now(),
					IsStale:    isStale,
				}

				// Always attempt submission (even if tip moved — network may still accept in a race; on testnet blocks are fast so we try)
				if isStale {
					m.statsMutex.Lock()
					m.stats.BlocksStale++
					m.statsMutex.Unlock()
				}
				{
					// Attempt submission (stale or not — let the network decide)
					submitErr := m.submitBlock(block, nonce)
					if submitErr != nil {
						log.Printf("❌ Error submitting block: %v", submitErr)
						m.broadcastLogMessage(fmt.Sprintf("❌ Error submitting block: %v", submitErr))
						if strings.Contains(submitErr.Error(), "rejected") {
							log.Printf("   Peer rejected the block — no reward. Check the reason above (e.g. invalid PoW, stale block, or wrong network).")
						}
						submittedBlock.BroadcastOk = false
						submittedBlock.ErrorMessage = submitErr.Error()
					} else {
						m.statsMutex.Lock()
						m.stats.BlocksFound++
						m.stats.BlocksAccepted++
						m.statsMutex.Unlock()

						submittedBlock.BroadcastOk = true
						if mode == "p2p" && p2pClient != nil {
							// Count broadcast peers
							submittedBlock.PeerCount = len(p2pClient.GetBroadcastPeers()) + 1 // +1 for primary
						}

						successMsg := "✅ Block relayed to P2P peer(s) (chain acceptance still unconfirmed)."
						log.Println(successMsg)
						m.broadcastLogMessage(successMsg)
						// Immediately refresh headers after successful submit so next round pivots to newest known tip.
						// Use tiny maxAge to force refresh without changing other call sites.
						if mode == "p2p" && p2pClient != nil {
							p2pClient.RefreshHeadersIfStale(1 * time.Nanosecond)
						}
						if network == "testnet" {
							log.Printf("   Testnet block height %d — check https://sochain.com/block/DOGETEST/%d", template.Height, template.Height)
							if isStale && blocksAhead > 0 {
								log.Printf("   ⚠️ Block was stale (network was %d blocks ahead). You only get paid if this block is on the best chain; usually it is orphaned and you get no reward.", blocksAhead)
							} else {
								log.Printf("   Relay success only means peers accepted the message. On a fast testnet, another miner often wins the same height first — explorer may show their coinbase, not yours (no payout).")
								log.Printf("   If that block shows another miner's address, our block was orphaned (network accepted their block first); only the accepted block gets the reward.")
							}
						} else {
							log.Printf("   Mainnet block height %d — check your payout address after the block is confirmed (≈1 block).", template.Height)
							if isStale && blocksAhead > 0 {
								log.Printf("   ⚠️ Block was stale (%d blocks behind tip); reward only if it gets in the chain.", blocksAhead)
							}
						}
					}

					// Record submitted block
					m.submittedMutex.Lock()
					m.submittedBlocks = append(m.submittedBlocks, submittedBlock)
					if len(m.submittedBlocks) > 100 {
						m.submittedBlocks = m.submittedBlocks[len(m.submittedBlocks)-100:]
					}
					m.submittedMutex.Unlock()

					// After block broadcast, add a brief cooling period.
					// On testnet (1-block/sec), a long cooldown means many orphaned blocks — keep it very short.
					// On mainnet, 30s is fine to avoid peer rate limiting.
					if mode == "p2p" && submitErr == nil {
						cooldownSecs := 30
						if network == "testnet" {
							cooldownSecs = 0 // testnet: no cooldown — blocks arrive every ~1s, ANY pause = orphaned
						}
						if cooldownSecs > 0 {
							log.Printf("⏳ Cooling period: waiting %ds after block broadcast...", cooldownSecs)
							time.Sleep(time.Duration(cooldownSecs) * time.Second)
						}
					}

					// Track block for P2P payment monitoring (no APIs, pure P2P)
					if m.paymentMonitor != nil {
						m.paymentMonitor.TrackSubmittedBlock(template.Height, submittedBlock.Hash)

						// Check for payment confirmation after a few blocks
						go func() {
							time.Sleep(2 * time.Minute) // Wait for 2+ confirmations
							if err := m.paymentMonitor.CheckForPayments(); err != nil {
								// Silently continue - will retry on next check
							}
							// DON'T broadcast here - 2-second ticker will pick up payment updates
						}()
					}
				}
			}
		}
	}
}

// createEmptyBlock creates a block with ONLY the coinbase transaction
// This ignores any mempool transactions to mine empty blocks efficiently
// logPayoutInfo: if true, logs payout address and coinbase value (only once per block)
func (m *Miner) createEmptyBlock(template *BlockTemplate, logPayoutInfo bool) (*Block, error) {
	// Note: We explicitly ignore template.Transactions - we only mine empty blocks
	// This avoids wasting time processing mempool transactions

	if m.isDebugLogging() {
		log.Printf("📦 BLOCK TEMPLATE: Height %d, %s", template.Height, FormatTemplateBitsForLog(template.Bits))
		log.Printf("   Previous block (BE):    %s", template.PreviousBlockHash[:16]+"...")
		log.Printf("   Version: %d, Timestamp: %d", template.Version, template.CurTime)
	}

	// Parse previous block hash
	prevBlock, err := hexTo32Bytes(template.PreviousBlockHash)
	if err != nil {
		return nil, fmt.Errorf("invalid previous block hash: %v", err)
	}

	// Parse bits (format: "1d00ffff" as hex string)
	bitsHex := template.Bits
	if len(bitsHex) != 8 {
		return nil, fmt.Errorf("bits must be 8 hex characters")
	}
	bitsBytes, err := hex.DecodeString(bitsHex)
	if err != nil {
		return nil, fmt.Errorf("invalid bits hex: %v", err)
	}
	if len(bitsBytes) != 4 {
		return nil, fmt.Errorf("bits must decode to 4 bytes")
	}
	// Bits are stored in little-endian format
	bits := binary.LittleEndian.Uint32(bitsBytes)

	// Create coinbase script (height + extra nonce + flags)
	// Bitcoin/Dogecoin CScript height encoding (BIP34): push the height as a minimal little-endian signed integer.
	// This is NOT a varint — it is OP_PUSHDATAn followed by n bytes of CScriptNum (sign-magnitude LE).
	// Getting this wrong causes nodes to reject the block with "bad-cb-height" even though PoW is valid.
	coinbaseScript := make([]byte, 0)
	{
		heightVal := template.Height
		var heightBytes []byte
		for heightVal > 0 {
			heightBytes = append(heightBytes, byte(heightVal&0xFF))
			heightVal >>= 8
		}
		// If the most-significant byte has its high bit set, append 0x00 to indicate positive
		if len(heightBytes) > 0 && (heightBytes[len(heightBytes)-1]&0x80) != 0 {
			heightBytes = append(heightBytes, 0x00)
		}
		if len(heightBytes) == 0 {
			heightBytes = []byte{0x00}
		}
		coinbaseScript = append(coinbaseScript, byte(len(heightBytes))) // OP_PUSHDATAn
		coinbaseScript = append(coinbaseScript, heightBytes...)
	}
	// Add extra nonce (4 bytes) - increment sequentially like real miners
	// This is much faster than crypto/rand and gives different merkle roots per block
	m.extraNonceMutex.Lock()
	extraNonceValue := m.extraNonce
	m.extraNonce++
	if m.extraNonce == 0 {
		m.extraNonce = 1 // Skip zero, start from 1
	}
	m.extraNonceMutex.Unlock()

	// Convert to 4-byte little-endian (reuse buffer if possible)
	// Note: We create new buffer here as it's appended to coinbaseScript
	// This is acceptable as it's only once per block (not in hot loop)
	extraNonce := make([]byte, 4)
	binary.LittleEndian.PutUint32(extraNonce, extraNonceValue)
	coinbaseScript = append(coinbaseScript, extraNonce...)
	// Add flags from coinbaseaux if available
	if len(template.CoinbaseScript) > 0 {
		coinbaseScript = append(coinbaseScript, template.CoinbaseScript...)
	}

	// Get payout address from config — use the address for the current network so testnet/mainnet payments are correct
	config := m.GetConfig()
	payoutAddr := config.PayoutAddr
	if config.Network == "testnet" && config.PayoutAddrTestnet != "" {
		payoutAddr = config.PayoutAddrTestnet
	} else if config.Network == "mainnet" && config.PayoutAddrMainnet != "" {
		payoutAddr = config.PayoutAddrMainnet
	}
	var payoutScript []byte

	if payoutAddr != "" {
		decodedScript, err := DecodeDogecoinAddress(payoutAddr)
		if err != nil {
			if logPayoutInfo {
				log.Printf("⚠️ Warning: Failed to decode payout address '%s': %v. Using default script.", payoutAddr, err)
			}
			// Fall back to default script if decoding fails
			payoutScript = []byte{0x76, 0xA9, 0x14, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x88, 0xAC}
		} else {
			payoutScript = decodedScript
			if logPayoutInfo {
				log.Printf("✅ Using payout address: %s (%s) (decoded to scriptPubKey)", payoutAddr, config.Network)
			}
		}
	} else {
		// Default script (incomplete - rewards won't go anywhere specific)
		payoutScript = []byte{0x76, 0xA9, 0x14, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x88, 0xAC}
		if logPayoutInfo {
			log.Printf("⚠️ Warning: No payout address configured for %s. Block rewards may not be claimable.", config.Network)
		}
	}

	// Create coinbase transaction
	// NOTE: Transaction version must always be 1 (standard). The block version is separate
	// and may include AuxPoW bits (e.g. 0x620000 on Dogecoin mainnet) — using it as the
	// tx version would cause nodes to reject the block with "bad-txns-version".
	coinbaseTx := &Transaction{
		Version:  1,
		LockTime: 0,
		Inputs: []TxInput{
			{
				PreviousOutput: OutPoint{
					Hash:  [32]byte{},
					Index: 0xFFFFFFFF,
				},
				ScriptSig: coinbaseScript,
				Sequence:  0xFFFFFFFF,
			},
		},
		Outputs: []TxOutput{
			{
				Value:        template.CoinbaseValue,
				ScriptPubKey: payoutScript,
			},
		},
	}

	// Calculate merkle root (for single transaction, it's just the hash)
	// Note: Merkle root is calculated from transactions, not from nonce
	// The nonce only affects the block header hash, not the merkle root
	merkleRoot, err := coinbaseTx.GetHash()
	if err != nil {
		return nil, fmt.Errorf("failed to compute merkle root: %v", err)
	}

	if m.isDebugLogging() {
		log.Printf("🌳 MERKLE ROOT: %s (SHA256(SHA256(coinbase tx)))", hex.EncodeToString(merkleRoot[:]))
		if txBytes, err := coinbaseTx.Serialize(); err == nil {
			log.Printf("   Coinbase tx: %d bytes, type: EMPTY BLOCK (no mempool transactions)", len(txBytes))
		}
	}

	// Log coinbase details for verification (only once per block)
	if logPayoutInfo {
		log.Printf("💰 Coinbase value: %.2f DOGE (%d koinu), Payout script: %d bytes",
			float64(template.CoinbaseValue)/1e8, template.CoinbaseValue, len(payoutScript))
	}

	// Create block with ONLY coinbase transaction (empty block - no mempool transactions)
	// This is intentional - we only mine empty blocks to maximize hash attempts
	// We explicitly ignore template.Transactions to avoid wasting time on mempool data
	block := &Block{
		Version:      int32(template.Version),
		PrevBlock:    prevBlock,
		MerkleRoot:   merkleRoot,
		Timestamp:    uint32(template.CurTime),
		Bits:         bits,
		Nonce:        0,
		AuxPow:       nil, // No merge-mining; will serialize minimal AuxPow in Serialize()
		Transactions: []*Transaction{coinbaseTx}, // Only coinbase - no other transactions
	}

	// Verify we're creating an empty block (only 1 transaction = coinbase)
	if len(block.Transactions) != 1 {
		return nil, fmt.Errorf("invalid block: expected only coinbase transaction, got %d", len(block.Transactions))
	}

	return block, nil
}

// verifyBlockPoW re-computes scrypt(header) and checks it meets the block's target.
// Returns (PoW hash in display order for logging, meetsTarget, error).
func (m *Miner) verifyBlockPoW(block *Block) (powHashDisplay string, meetsTarget bool, err error) {
	header := block.SerializeHeader()
	if len(header) != 80 {
		return "", false, fmt.Errorf("header length %d, want 80", len(header))
	}
	// scryptHash returns bytes in little-endian uint256 format directly - no reversal needed
	hashLE := m.miner.scryptHash(header)
	targetBig := BitsToTarget(block.Bits)
	if targetBig == nil || targetBig.Sign() <= 0 {
		// For display, reverse the LE hash to show in display order
		hashDisplay := make([]byte, 32)
		copy(hashDisplay, hashLE)
		reverseBytesInPlace(hashDisplay)
		return HashToDisplayHex(hashDisplay), false, fmt.Errorf("invalid bits in block")
	}
	targetBigBytes := targetBig.Bytes()
	targetBytesLE := make([]byte, 32)
	copy(targetBytesLE[32-len(targetBigBytes):], targetBigBytes)
	reverseBytesInPlace(targetBytesLE)
	// hashLE is already in little-endian uint256 format from scryptHash
	meetsTarget = hashMeetsTargetLE(hashLE, targetBytesLE)

	// Detailed hash comparison logging
	if m.isDebugLogging() {
		hashDisplay := make([]byte, 32)
		copy(hashDisplay, hashLE)
		reverseBytesInPlace(hashDisplay)
		log.Printf("🔐 POW VERIFICATION (Educational):")
		log.Printf("   Header (80 bytes): %s", hex.EncodeToString(header[:16])+"...")
		log.Printf("   Scrypt(header) computed: %s (little-endian, native uint256 format)", hex.EncodeToString(hashLE)[:32])
		log.Printf("   Hash display order: %s (reversed for block explorers)", hex.EncodeToString(hashDisplay)[:32])
		log.Printf("   ")
		log.Printf("   Target from Bits 0x%08x:", block.Bits)
		log.Printf("     BE format (RPC):  %s", hex.EncodeToString(targetBigBytes))
		log.Printf("     LE format (cmp):  %s", hex.EncodeToString(targetBytesLE)[:32])
		log.Printf("   ")
		log.Printf("   Comparison: hash %s target (meets=%v) ✓", map[bool]string{true: "<=", false: ">"}[meetsTarget], meetsTarget)
		log.Printf("   Nonce: %d (0x%08x)", block.Nonce, block.Nonce)
	}

	// Return hash in display order for logging
	hashDisplay := make([]byte, 32)
	copy(hashDisplay, hashLE)
	reverseBytesInPlace(hashDisplay)
	return HashToDisplayHex(hashDisplay), meetsTarget, nil
}

// runDiagnosticTest verifies mining infrastructure is working correctly
func (m *Miner) runDiagnosticTest() {
	log.Printf("🔧 DIAGNOSTIC: Testing mining infrastructure...")

	// Test 1: Verify scrypt hash function
	header := make([]byte, 80)
	binary.LittleEndian.PutUint32(header[0:4], 2)    // Version
	binary.LittleEndian.PutUint32(header[68:72], 1) // Timestamp
	binary.LittleEndian.PutUint32(header[72:76], 0x1b0404cb) // Bits (testnet min diff)

	hash := m.miner.scryptHash(header)
	log.Printf("✓ Scrypt hash computed: %s (len=%d, little-endian uint256 format)", hex.EncodeToString(hash)[:32], len(hash))

	// Test 2: Verify bits to target conversion
	bits := uint32(0x1b0404cb) // Testnet min difficulty
	target := BitsToTarget(bits)
	targetBytes := target.Bytes()
	targetLE := make([]byte, 32)
	copy(targetLE[32-len(targetBytes):], targetBytes)
	reverseBytesInPlace(targetLE)
	log.Printf("✓ Target computed from bits 0x%08x: %s", bits, hex.EncodeToString(targetLE)[:32])

	// Test 3: Simulate finding a block (brute force low difficulty)
	log.Printf("🔧 Testing mining with 10,000 nonces...")
	noncesTestedDiagnostic := uint64(0)
	found := false
	for i := uint32(0); i < 10000; i++ {
		binary.LittleEndian.PutUint32(header[76:80], i)
		testHashLE := m.miner.scryptHash(header) // Returns LE format directly
		noncesTestedDiagnostic++

		if hashMeetsTargetLE(testHashLE, targetLE) {
			log.Printf("✓ DIAGNOSTIC: Found block at nonce %d after %d attempts!", i, noncesTestedDiagnostic)
			found = true
			break
		}
	}

	if !found {
		log.Printf("⚠️  DIAGNOSTIC: No blocks found in 10k nonces (expected on mainnet, normal on testnet with high difficulty)")
		log.Printf("   If difficulty is very high, this is normal. Enable Debug Logging to see target values.")
	}

	log.Printf("✓ DIAGNOSTIC: Infrastructure test complete")
}

func (m *Miner) submitBlock(block *Block, nonce uint32) error {
	// Create a copy of the block to avoid modifying the original
	blockCopy := *block
	blockCopy.Nonce = nonce

	// DEBUG: Log the exact 80-byte header being submitted for verification
	headerAtSubmit := blockCopy.SerializeHeader()
	logHeaderDebug("[SUBMIT]", headerAtSubmit)

	// Verify the block has the coinbase transaction with payout address
	if len(blockCopy.Transactions) == 0 {
		return fmt.Errorf("block has no transactions")
	}

	coinbaseTx := blockCopy.Transactions[0]
	if len(coinbaseTx.Outputs) == 0 {
		return fmt.Errorf("coinbase transaction has no outputs")
	}

	// Verify payout script is set (not default empty script)
	payoutScript := coinbaseTx.Outputs[0].ScriptPubKey
	if len(payoutScript) < 5 {
		return fmt.Errorf("coinbase payout script is invalid (too short)")
	}

	// Serialize block with winning nonce (used for RPC submission and debug)
	blockHex, err := blockCopy.Serialize()
	if err != nil {
		return fmt.Errorf("failed to serialize block: %v", err)
	}

	// Safely get clients and connection mode
	m.configMutex.RLock()
	mode := m.config.ConnectionMode
	rpcClient := m.rpcClient
	p2pClient := m.p2pClient
	m.configMutex.RUnlock()

	switch mode {
	case "p2p":
		// CPU Scrypt re-verification of the exact serialized block header before broadcast.
		// This catches any mismatch between what the miner verified and what actually gets submitted.
		prevDisplay := HashToDisplayHex(blockCopy.PrevBlock[:])
		powHashDisplay, meetsTarget, powErr := m.verifyBlockPoW(&blockCopy)
		if powErr != nil {
			log.Printf("⚠️ submitBlock: PoW re-verify error (nonce=%d): %v — aborting submission", nonce, powErr)
			return powErr
		}
		if !meetsTarget {
			log.Printf("⚠️ submitBlock: CPU Scrypt of submitted header does NOT meet target (hash=%s) — aborting submission (false positive from miner)", powHashDisplay)
			return fmt.Errorf("submitted block PoW invalid: hash %s does not meet target", powHashDisplay)
		}
		log.Printf("✅ submitBlock: CPU Scrypt verified submitted header (hash=%s meets target)", powHashDisplay)
		log.Printf("📤 P2P submit: version=0x%08x bits=0x%08x prevBlock=%s nonce=%d", blockCopy.Version, blockCopy.Bits, prevDisplay, nonce)
		// Lazy-init P2P client if needed
		if p2pClient == nil {
			m.configMutex.Lock()
			if m.p2pClient == nil {
				net := m.config.Network
				if net == "" {
					net = "mainnet"
				}
				m.p2pClient = NewP2PClient(net)
			}
			p2pClient = m.p2pClient
			m.configMutex.Unlock()
		}
		if p2pClient == nil {
			return fmt.Errorf("P2P client not available")
		}
		log.Printf("📤 Submitting block with nonce %d via P2P...", nonce)
		if err := p2pClient.BroadcastBlock(&blockCopy); err != nil {
			return fmt.Errorf("P2P block broadcast failed: %v", err)
		}
	default: // "rpc"
		if rpcClient == nil {
			return fmt.Errorf("RPC client not available")
		}
		log.Printf("📤 Submitting block with nonce %d to Dogecoin network via RPC...", nonce)
		if err := rpcClient.SubmitBlock(blockHex); err != nil {
			return fmt.Errorf("block submission failed: %v", err)
		}
	}

	log.Printf("✅ Block relay finished (mode=%s). Mining a block ≠ automatic reward — coinbase pays only if this block becomes part of the best chain.", mode)
	return nil
}

// expectedSecondsPerBlockAt returns average seconds to find one block at given difficulty and hashrate (H/s).
// Formula: expected hashes = 2^32 * difficulty (Dogecoin/Bitcoin), so time = (2^32 * diff) / hashrate.
func expectedSecondsPerBlockAt(difficulty float64, hashrateHps float64) float64 {
	if difficulty <= 0 || hashrateHps <= 0 {
		return 0
	}
	const hashesPerBlockAtDiff1 = 4294967296.0 // 2^32
	return (hashesPerBlockAtDiff1 * difficulty) / hashrateHps
}

func (m *Miner) GetStats() *MiningStats {
	m.statsMutex.RLock()
	defer m.statsMutex.RUnlock()

	stats := *m.stats
	if m.p2pClient != nil {
		stats.P2PPeer = m.p2pClient.GetPeerAddr()
		info := m.p2pClient.GetPeerInfo()
		stats.P2PPeerInfo = &info
	}

	// Get network, address, and device type from config
	m.configMutex.RLock()
	stats.Network = m.config.Network
	stats.PayoutAddress = m.config.PayoutAddr
	stats.DeviceType = m.config.DeviceType
	m.configMutex.RUnlock()

	// Get payment tracking info from P2P payment monitor
	if m.paymentMonitor != nil {
		paymentStats := m.paymentMonitor.GetStats()
		stats.PaymentTracking = &paymentStats
	}

	diff := 0.0
	if stats.CurrentBlock != nil && stats.CurrentBlock.Difficulty > 0 {
		diff = stats.CurrentBlock.Difficulty
	}
	stats.ExpectedSecondsPerBlock = expectedSecondsPerBlockAt(diff, stats.HashRate)
	return &stats
}

func (m *Miner) broadcastStats() {
	// Skip broadcast if no clients connected (no point doing the work)
	m.clientsMutex.RLock()
	clientCount := len(m.clients)
	m.clientsMutex.RUnlock()

	if clientCount == 0 {
		return // No clients, skip expensive operations
	}

	stats := m.GetStats()
	data, err := json.Marshal(stats)
	if err != nil {
		return
	}

	// Must serialize websocket writes - gorilla websocket is not thread-safe
	m.wsWriteMutex.Lock()
	defer m.wsWriteMutex.Unlock()

	m.clientsMutex.RLock()
	defer m.clientsMutex.RUnlock()

	for conn := range m.clients {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			// Don't log errors here - too noisy and can slow down mining
			// Client will reconnect automatically
		}
	}
}

func (m *Miner) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	m.clientsMutex.Lock()
	m.clients[conn] = true
	m.clientsMutex.Unlock()

	// Send initial stats
	stats := m.GetStats()
	m.wsWriteMutex.Lock()
	err = conn.WriteJSON(stats)
	m.wsWriteMutex.Unlock()
	if err != nil {
		log.Printf("Error sending initial stats: %v", err)
	}

	// Keep connection alive
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}

	m.clientsMutex.Lock()
	delete(m.clients, conn)
	m.clientsMutex.Unlock()
}

func main() {
	// Global panic recovery to log crashes before exit
	defer func() {
		if r := recover(); r != nil {
			crashMsg := fmt.Sprintf("❌ FATAL PANIC: %v\n", r)
			log.Print(crashMsg)

			// Write crash log to file
			crashFile := "crash.log"
			f, err := os.OpenFile(crashFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err == nil {
				timestamp := time.Now().Format("2006-01-02 15:04:05")
				fmt.Fprintf(f, "\n=== CRASH at %s ===\n%s\n", timestamp, crashMsg)
				f.Close()
				log.Printf("Crash details saved to %s", crashFile)
			}

			// Keep window open so user can see the error
			fmt.Println("\n❌ Application crashed. Press Enter to exit...")
			fmt.Scanln()
		}
	}()

	// Maximize CPU utilization - use ALL available cores for parallel mining
	numCPU := runtime.NumCPU()
	runtime.GOMAXPROCS(numCPU)
	log.Printf("🐕 Ðoge Lucky Mining — coded with love for the Dogecoin community (open source)")
	log.Printf("   Author: Paulo Vidal (Dogecoin Foundation Dev) | x.com/inevitable360 | github.com/qlpqlp")
	log.Printf("🚀 System: %d CPU cores detected, all will be used for mining", numCPU)

	var (
		rpcURL  = flag.String("rpc-url", "http://localhost:22555", "Dogecoin RPC URL")
		rpcUser = flag.String("rpc-user", "dogecoin", "Dogecoin RPC username")
		rpcPass = flag.String("rpc-pass", "password", "Dogecoin RPC password")
		port    = flag.String("port", "42069", "Web server port")
	)
	flag.Parse()

	// Capture log output for web UI (same as console)
	logCapture = &logCaptureBuffer{lines: make([]string, 0, maxLogLines)}
	log.SetOutput(io.MultiWriter(os.Stderr, logCapture))

	// Try to load saved configuration first
	var savedConfig *MinerConfig
	if config, err := loadConfigFromFile(); err == nil {
		savedConfig = config
		hadConfigAtStartup = true
		log.Println("Loaded saved configuration from file")
		// Use saved config if available, otherwise use command-line args
		if savedConfig.RPCURL != "" {
			*rpcURL = savedConfig.RPCURL
		}
		if savedConfig.RPCUser != "" {
			*rpcUser = savedConfig.RPCUser
		}
		if savedConfig.RPCPass != "" {
			*rpcPass = savedConfig.RPCPass
		}
		if savedConfig.PayoutAddr != "" {
			// PayoutAddr doesn't have a command-line flag, so we'll use it from config
		}
	} else {
		log.Printf("No saved configuration found (this is normal on first run): %v", err)
	}

	// Create miner with config (from file or command-line)
	var miner *Miner
	var err error

	if savedConfig != nil && savedConfig.ConnectionMode == "p2p" {
		// P2P-only mode: create miner without requiring RPC; no connection test or warning
		miner, err = newMinerP2POnly(savedConfig)
		if err != nil {
			log.Printf("Warning: Failed to create miner (P2P mode): %v", err)
			return
		}
		log.Println("P2P mode: no RPC required — connect and mine via the network")
	} else {
		miner, err = NewMiner(*rpcURL, *rpcUser, *rpcPass, func() string {
			if savedConfig != nil && savedConfig.PayoutAddr != "" {
				return savedConfig.PayoutAddr
			}
			return ""
		}())
		if err != nil {
			log.Printf("Warning: Failed to connect with default settings: %v", err)
			log.Printf("You can configure RPC settings via the web interface")
			// Create miner anyway - user can configure via web UI
			config := &MinerConfig{
				RPCURL:  *rpcURL,
				RPCUser: *rpcUser,
				RPCPass: *rpcPass,
				PayoutAddr: func() string {
					if savedConfig != nil {
						return savedConfig.PayoutAddr
					}
					return ""
				}(),
				Network: func() string {
					if savedConfig != nil && savedConfig.Network != "" {
						return savedConfig.Network
					}
					return "testnet"
				}(),
				ConnectionMode: "p2p", // first-run default: testnet + P2P (no node required)
				P2PCheckpoint: func() string {
					if savedConfig != nil && savedConfig.P2PCheckpoint != "" {
						return savedConfig.P2PCheckpoint
					}
					return "42799352" // newest testnet checkpoint (Block 42,799,352) when defaulting to testnet
				}(),
				DeviceType: func() string {
					if savedConfig != nil && savedConfig.DeviceType != "" {
						return savedConfig.DeviceType
					}
					return "gpu"
				}(),
				ThreadCount: func() int {
					if savedConfig != nil && savedConfig.ThreadCount > 0 {
						return savedConfig.ThreadCount
					}
					return 64
				}(),
				MaxHashes: func() uint64 {
					if savedConfig != nil && savedConfig.MaxHashes > 0 {
						return savedConfig.MaxHashes
					}
					return 1000000 // extreme
				}(),
				MiningIntensity: func() string {
					if savedConfig != nil && savedConfig.MiningIntensity != "" {
						return savedConfig.MiningIntensity
					}
					return "extreme"
				}(),
			}
			miner = &Miner{
				config: config,
				stats: &MiningStats{
					StartTime:   time.Now(),
					Status:      "stopped",
					LastUpdate:  time.Now(),
					LogMessages: make([]string, 0, 100), // Pre-allocate for 100 messages
				},
				stopChan:          make(chan struct{}),
				clients:           make(map[*websocket.Conn]bool),
				miner:             NewScryptMinerWithConfig(config.MaxHashes, config.ThreadCount),
				gpuMiner:          NewGPUMiner(config.MaxHashes, config.ThreadCount),
				enableHashLogging: true,
			}
			// Apply default nonce strategy so options work consistently (saved config applies via UpdateConfig on next load)
			miner.miner.SetNonceStrategy("sequential", "calculated", false)
			miner.gpuMiner.SetNonceStrategy("sequential", "calculated", false)
			// Do not save yet — first run; user must configure (e.g. payout) and save from UI
		} else {
			log.Println("Successfully connected to Dogecoin node with default settings")
			if savedConfig != nil {
				if err := miner.UpdateConfig(savedConfig); err != nil {
					log.Printf("Warning: Failed to apply saved configuration: %v", err)
				}
			}
		}
	}

	// Serve embedded web UI (bundled in exe so no external web folder needed)
	webRoot, _ := fs.Sub(embeddedWeb, "web")
	http.Handle("/", http.FileServer(http.FS(webRoot)))
	http.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		lines := logCapture.Lines()
		json.NewEncoder(w).Encode(map[string]interface{}{"lines": lines})
	})
	http.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		stats := miner.GetStats()
		// Add header fetcher stats
		statsMap := make(map[string]interface{})
		statsJSON, _ := json.Marshal(stats)
		json.Unmarshal(statsJSON, &statsMap)

		// Safely get header fetcher stats
		if miner.headerFetcher != nil {
			statsMap["headersFetched"] = miner.headerFetcher.GetHeadersCount()
			statsMap["lastHeaderHeight"] = miner.headerFetcher.GetLastHeight()
		} else {
			statsMap["headersFetched"] = 0
			statsMap["lastHeaderHeight"] = -1
		}

		json.NewEncoder(w).Encode(statsMap)
	})

	// API endpoint for submitted blocks history
	http.HandleFunc("/api/submitted-blocks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		miner.submittedMutex.RLock()
		blocks := make([]SubmittedBlock, len(miner.submittedBlocks))
		copy(blocks, miner.submittedBlocks)
		miner.submittedMutex.RUnlock()

		// Reverse order (newest first)
		for i, j := 0, len(blocks)-1; i < j; i, j = i+1, j-1 {
			blocks[i], blocks[j] = blocks[j], blocks[i]
		}

		json.NewEncoder(w).Encode(blocks)
	})

	http.HandleFunc("/api/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := miner.Start(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		miner.Stop()
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			cfg := miner.GetConfig()
			out := make(map[string]interface{})
			cfgJSON, _ := json.Marshal(cfg)
			json.Unmarshal(cfgJSON, &out)
			out["firstRun"] = !hadConfigAtStartup
			json.NewEncoder(w).Encode(out)
		} else if r.Method == "POST" {
			var config MinerConfig
			if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
				http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
				return
			}

			// Check if miner is currently running
			miner.statsMutex.RLock()
			wasRunning := miner.stats.Status == "running"
			miner.statsMutex.RUnlock()

// Stop miner if running to apply new configuration
		if wasRunning {
			log.Println("⏹️ Stopping miner to apply new configuration...")
			miner.broadcastLogMessage("⏹️ Stopping mining to apply configuration changes...")
			miner.Stop()
			// Wait briefly for clean shutdown
			time.Sleep(200 * time.Millisecond)
			miner.statsMutex.Lock()
			miner.stats.Status = "stopped"
			miner.statsMutex.Unlock()
		}

		// Require payout address for mainnet and testnet
			payout := config.PayoutAddr
			if config.Network == "testnet" && config.PayoutAddrTestnet != "" {
				payout = config.PayoutAddrTestnet
			}
			if config.Network == "mainnet" && config.PayoutAddrMainnet != "" {
				payout = config.PayoutAddrMainnet
			}
			if strings.TrimSpace(payout) == "" {
				http.Error(w, "Payout address is required for mainnet and testnet", http.StatusBadRequest)
				return
			}
			// Update configuration
			if err := miner.UpdateConfig(&config); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			// Restart miner if it was running
			if wasRunning {
				log.Printf("🔄 Restarting miner with new configuration (Threads: %d, MaxHashes: %d, Intensity: %s, Device: %s)...", config.ThreadCount, config.MaxHashes, config.MiningIntensity, config.DeviceType)
				miner.broadcastLogMessage(fmt.Sprintf("🔄 Restarting with new config: Threads=%d, Intensity=%s, Device=%s", config.ThreadCount, config.MiningIntensity, config.DeviceType))
				go func() {
					// Wait longer to ensure complete shutdown before restarting
					time.Sleep(1 * time.Second)
					log.Printf("📍 Attempting to start miner with new configuration...")
					if err := miner.Start(); err != nil {
						log.Printf("❌ Failed to restart miner: %v", err)
						miner.broadcastLogMessage(fmt.Sprintf("❌ Failed to restart miner: %v", err))
					} else {
						log.Println("✅ Miner restarted with new configuration (changes applied)")
						miner.broadcastLogMessage("✅ Miner restarted with new configuration")
					}
				}()
			}

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "success", "restarted": fmt.Sprintf("%t", wasRunning)})
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
	http.HandleFunc("/api/test-connection", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var config MinerConfig
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
			return
		}

		testClient := NewRPCClient(config.RPCURL, config.RPCUser, config.RPCPass)
		if err := testClient.TestConnection(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Connection successful"})
	})
	http.HandleFunc("/api/generate-address", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Network string `json:"network"` // "mainnet" or "testnet"
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		network := strings.ToLower(strings.TrimSpace(req.Network))
		if network != "mainnet" && network != "testnet" {
			network = "testnet"
		}
		chain := doge.ChainFromTestNetFlag(network == "testnet")
		priv, err := doge.GenerateECPrivKey()
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to generate key: %v", err), http.StatusInternalServerError)
			return
		}
		pub := doge.ECPubKeyFromECPrivKey(priv)
		addr, err := doge.PubKeyToP2PKH(pub[:], chain)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to derive address: %v", err), http.StatusInternalServerError)
			return
		}
		wif := doge.EncodeECPrivKeyWIF(priv, chain)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"address":       string(addr),
			"privateKeyWIF": wif,
		})
	})
	http.HandleFunc("/api/hash-logging", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
			return
		}

		miner.SetHashLogging(req.Enabled)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	})
	http.HandleFunc("/ws", miner.handleWebSocket)

	// Bind to requested port; if in use, try next ports so the app doesn't crash when double-clicked
	for _, p := range []string{*port, "42070", "42071", "42072", "42073", "42074"} {
		listener, err := net.Listen("tcp", ":"+p)
		if err != nil {
			if strings.Contains(err.Error(), "address already in use") || strings.Contains(err.Error(), "Only one usage") {
				log.Printf("Port %s in use, trying next...", p)
				continue
			}
			log.Fatalf("Server error: %v", err)
		}
		if addr, ok := listener.Addr().(*net.TCPAddr); ok && addr != nil {
			*port = fmt.Sprintf("%d", addr.Port)
		} else {
			*port = p
		}
		log.Printf("Starting web server on port %s", *port)
		log.Printf("Open http://localhost:%s in your browser", *port)
		go func() {
			time.Sleep(1 * time.Second)
			url := "http://localhost:" + *port
			switch runtime.GOOS {
			case "windows":
				_ = exec.Command("cmd", "/c", "start", url).Start()
			case "darwin":
				_ = exec.Command("open", url).Start()
			default:
				_ = exec.Command("xdg-open", url).Start()
			}
		}()
		if err := http.Serve(listener, nil); err != nil {
			log.Printf("Server error: %v", err)
		}
		return
	}
	log.Fatalf("Could not bind to port %s or alternatives (42070-42074). Close other instances or free the port.", *port)
}
