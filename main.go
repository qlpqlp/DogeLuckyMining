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
	configFileName    = "config.json"
	chainTipFileName  = "chain_tip.json"
	hadConfigAtStartup bool   // true if config file existed on startup (so we don't show first-run modal)
	logCapture        *logCaptureBuffer
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
	rpcClient     *RPCClient
	p2pClient     *P2PClient
	miner         *ScryptMiner
	gpuMiner      *GPUMiner
	headerFetcher *HeaderFetcher
	paymentMonitor *P2PPaymentMonitor // P2P-based payment tracking (no APIs)
	stats         *MiningStats
	statsMutex    sync.RWMutex
	config        *MinerConfig
	configMutex   sync.RWMutex
	stopChan      chan struct{}
	wg            sync.WaitGroup
	clients       map[*websocket.Conn]bool
	clientsMutex  sync.RWMutex
	enableHashLogging bool // Toggle for hash attempt logging
	logMutex      sync.RWMutex
	extraNonce    uint32 // Sequential extra nonce counter (like real miners)
	extraNonceMutex sync.Mutex // Protects extraNonce increment
	submittedBlocks []SubmittedBlock // Track submitted blocks
	submittedMutex  sync.RWMutex
}

type MinerConfig struct {
	RPCURL      string `json:"rpcUrl"`
	RPCUser     string `json:"rpcUser"`
	RPCPass     string `json:"rpcPass"`
	PayoutAddr  string `json:"payoutAddr"` // active address for current network (used by miner)
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
	P2PPeerOverride        string `json:"p2pPeerOverride"`         // Legacy: applies to current network (deprecated, use network-specific fields)
	P2PPeerOverrideMainnet string `json:"p2pPeerOverrideMainnet"`  // Mainnet-specific peer override (e.g. "seed.multidoge.org:22556")
	P2PPeerOverrideTestnet string `json:"p2pPeerOverrideTestnet"`  // Testnet-specific peer override (e.g. "testseed.jrn.me.uk:44556")
	// Mining performance settings
	DeviceType     string `json:"deviceType"`     // "cpu" (optimized parallel CPU mining)
	ThreadCount    int    `json:"threadCount"`    // Number of parallel mining threads (1-16)
	MaxHashes      uint64 `json:"maxHashes"`      // Max hashes per block template (1000-1000000)
	MiningIntensity string `json:"miningIntensity"` // "low", "medium", "high", "extreme"
	// Nonce search strategy settings
	NonceSearchMode string `json:"nonceSearchMode"` // "forward", "backward", "sequential" (default)
	NonceStartMode  string `json:"nonceStartMode"`  // "random", "calculated" (from block metadata)
	UseStride       bool   `json:"useStride"`       // Use stride-based iteration for multi-threading
}

type SubmittedBlock struct {
	Height        int64     `json:"height"`
	Hash          string    `json:"hash"`
	Nonce         uint32    `json:"nonce"`
	SubmitTime    time.Time `json:"submit_time"`
	IsStale       bool      `json:"is_stale"`
	BroadcastOk   bool      `json:"broadcast_ok"`
	PeerCount     int       `json:"peer_count"`
	ErrorMessage  string    `json:"error_message,omitempty"`
}

type MiningStats struct {
	HashRate               float64   `json:"hashRate"`
	TotalHashes            uint64    `json:"totalHashes"`
	BlocksFound            uint64    `json:"blocksFound"`
	BlocksAccepted         uint64    `json:"blocksAccepted"`  // Blocks successfully broadcast
	BlocksStale            uint64    `json:"blocksStale"`     // Blocks detected as stale
	SharesSubmitted        uint64    `json:"sharesSubmitted"`
	StartTime              time.Time `json:"startTime"`
	CurrentBlock           *BlockInfo `json:"currentBlock"`
	Status                 string    `json:"status"`
	LastUpdate             time.Time `json:"lastUpdate"`
	LogMessages            []string  `json:"logMessages"` // Recent log messages for web interface
	P2PPeer                string    `json:"p2pPeer"`    // P2P mode: connected peer address
	P2PPeerInfo            *PeerInfo `json:"p2pPeerInfo,omitempty"` // P2P peer version/subversion from version message
	Network                string    `json:"network"`    // "mainnet" or "testnet"
	PayoutAddress          string    `json:"payoutAddress"`  // Current payout address
	PaymentTracking        *P2PMonitorStats `json:"paymentTracking,omitempty"` // P2P payment tracking (no APIs)
	ExpectedSecondsPerBlock float64  `json:"expectedSecondsPerBlock"` // At current diff/hashrate (0 = unknown)
	BlockAttemptSeconds    float64  `json:"blockAttemptSeconds"`     // Time spent mining last block attempt (for monitoring)
}

type BlockInfo struct {
	Height    int64  `json:"height"`
	Hash      string `json:"hash"`
	Difficulty float64 `json:"difficulty"`
	Target     string `json:"target"`
}

func NewMiner(rpcURL, rpcUser, rpcPass, payoutAddr string) (*Miner, error) {
	config := &MinerConfig{
		RPCURL:          rpcURL,
		RPCUser:         rpcUser,
		RPCPass:         rpcPass,
		PayoutAddr:      payoutAddr,
		Network:         "mainnet",    // Default to mainnet
		ConnectionMode:  "rpc",        // Default to RPC mode
		DeviceType:      "cpu",        // Default to CPU
		ThreadCount:     1,            // Default to single thread
		MaxHashes:       100000,       // Default max hashes per block
		MiningIntensity: "medium",     // Default intensity
		NonceSearchMode: "sequential", // Default: sequential from 0
		NonceStartMode:  "calculated", // Default: calculate from block metadata
		UseStride:       false,        // Default: range-based (not stride)
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
	gpuMiner := NewGPUMiner(config.MaxHashes, config.ThreadCount)

	// Initialize P2P payment monitor (no external APIs)
	// Create separate P2P client for payment monitor to avoid race conditions
	var paymentMonitor *P2PPaymentMonitor
	if p2pClient != nil {
		monitorP2PClient := NewP2PClient(config.Network)
		paymentMonitor = NewP2PPaymentMonitor(monitorP2PClient, config.PayoutAddr, config.Network)
		go paymentMonitor.StartMonitoring(30 * time.Second) // Check every 30 seconds
	}

	return &Miner{
		rpcClient:     rpcClient,
		p2pClient:     p2pClient,
		miner:         miner,
		gpuMiner:      gpuMiner,
		headerFetcher: headerFetcher,
		paymentMonitor: paymentMonitor,
		config:        config,
		stats: &MiningStats{
			StartTime:  time.Now(),
			Status:     "stopped",
			LastUpdate: time.Now(),
			LogMessages: make([]string, 0, 100), // Pre-allocate for 100 messages
			PayoutAddress: config.PayoutAddr,
		},
		stopChan: make(chan struct{}),
		clients:  make(map[*websocket.Conn]bool),
		enableHashLogging: false, // Disabled by default for high-speed runs (can enable via UI)
		extraNonce: 1, // Start from 1 (skip zero) - sequential increment like real miners
		submittedBlocks: make([]SubmittedBlock, 0),
	}, nil
}

// newMinerP2POnly creates a miner in P2P-only mode (no RPC client or connection test).
// Used at startup when saved config has ConnectionMode "p2p".
func newMinerP2POnly(config *MinerConfig) (*Miner, error) {
	if config == nil {
		config = &MinerConfig{ConnectionMode: "p2p"}
	}
	config.ConnectionMode = "p2p"
	if config.DeviceType == "" {
		config.DeviceType = "cpu"
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
	
	// Initialize P2P payment monitor (no external APIs)
	// Create separate P2P client for payment monitor to avoid race conditions
	monitorP2PClient := NewP2PClient(config.Network)
	paymentMonitor := NewP2PPaymentMonitor(monitorP2PClient, config.PayoutAddr, config.Network)
	go paymentMonitor.StartMonitoring(30 * time.Second) // Check every 30 seconds
	
	return &Miner{
		rpcClient:     nil,
		p2pClient:     p2pClient,
		headerFetcher: nil,
		miner:         NewScryptMinerWithConfig(config.MaxHashes, config.ThreadCount),
		gpuMiner:      NewGPUMiner(config.MaxHashes, config.ThreadCount),
		paymentMonitor: paymentMonitor,
		config:        config,
		stats: &MiningStats{
			StartTime:   time.Now(),
			Status:      "stopped",
			LastUpdate:  time.Now(),
			LogMessages: make([]string, 0, 100), // Pre-allocate for 100 messages
			PayoutAddress: config.PayoutAddr,
		},
		stopChan:         make(chan struct{}),
		clients:          make(map[*websocket.Conn]bool),
		enableHashLogging: false,
		extraNonce:       1,
		submittedBlocks: make([]SubmittedBlock, 0),
	}, nil
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
	if config.DeviceType == "" {
		config.DeviceType = "cpu"
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

	// Update payment monitor if network, address, or P2P client changed
	// IMPORTANT: Payment monitor needs its OWN P2P client to avoid race conditions
	needsNewMonitor := false
	m.configMutex.RLock()
	if m.config.Network != config.Network || m.config.PayoutAddr != config.PayoutAddr {
		needsNewMonitor = true
	}
	m.configMutex.RUnlock()
	
	if needsNewMonitor && newP2PClient != nil {
		// Stop old payment monitor if exists
		if m.paymentMonitor != nil {
			m.paymentMonitor.Stop()
		}
		
		// Create separate P2P client for payment monitor (avoids race conditions with main miner)
		monitorP2PClient := NewP2PClient(config.Network)
		if peerOverride := config.P2PPeerOverride; config.Network == "testnet" && config.P2PPeerOverrideTestnet != "" {
			monitorP2PClient.SetPeerOverride(config.P2PPeerOverrideTestnet)
		} else if config.Network == "mainnet" && config.P2PPeerOverrideMainnet != "" {
			monitorP2PClient.SetPeerOverride(config.P2PPeerOverrideMainnet)
		} else if peerOverride != "" {
			monitorP2PClient.SetPeerOverride(peerOverride)
		}
		newMonitor := NewP2PPaymentMonitor(monitorP2PClient, config.PayoutAddr, config.Network)
		go newMonitor.StartMonitoring(30 * time.Second)
		m.paymentMonitor = newMonitor
		log.Printf("💰 P2P payment monitor restarted for %s (%s)", config.PayoutAddr, config.Network)
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
	Height  int64  `json:"height"`
	HashHex string `json:"hash"`
}

// loadSavedTip loads the last known block tip from disk. Returns height, hash (hex), true if found and valid.
func loadSavedTip(network string) (height int64, hashHex string, ok bool) {
	if network == "" {
		network = "mainnet"
	}
	data, err := os.ReadFile(getTipPath(network))
	if err != nil {
		return 0, "", false
	}
	var tip savedChainTip
	if err := json.Unmarshal(data, &tip); err != nil {
		return 0, "", false
	}
	if tip.Height < 0 || len(tip.HashHex) != 64 {
		return 0, "", false
	}
	return tip.Height, tip.HashHex, true
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

// saveTip persists the current chain tip so the miner can resume from it on next start.
// Only logs when the tip actually changed (avoids repeating the same message every poll).
func saveTip(height int64, hashHex string, network string) {
	if network == "" {
		network = "mainnet"
	}
	prevHeight, prevHash, hadPrev := loadSavedTip(network)
	if hadPrev && prevHeight == height && prevHash == hashHex {
		return // unchanged, skip write and log
	}
	tip := savedChainTip{Height: height, HashHex: hashHex}
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

	// Payout address is required for mainnet and testnet
	payout := config.PayoutAddr
	if config.Network == "testnet" && config.PayoutAddrTestnet != "" {
		payout = config.PayoutAddrTestnet
	}
	if config.Network == "mainnet" && config.PayoutAddrMainnet != "" {
		payout = config.PayoutAddrMainnet
	}
	if strings.TrimSpace(payout) == "" {
		return errors.New("payout address is required for mainnet and testnet — set it in Configuration")
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

	m.wg.Add(1)
	go m.miningLoop()
	
	log.Println("Miner started")
	m.broadcastLogMessage("✅ Miner initialized and running")
	return nil
}

func (m *Miner) Stop() {
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
				}

				if p2pClient == nil {
					log.Printf("P2P client not available, retrying...")
					time.Sleep(5 * time.Second)
					continue
				}

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
				
				// Priority: Explicit checkpoint > Saved tip > Genesis
				// If user selected a checkpoint (including "0" for genesis), always use it
				// Only use saved tip if no checkpoint is configured
				if checkpointID != "" {
					_ = p2pClient.SetStartCheckpointByID(checkpointID)
					if time.Since(lastCheckpointLog) > 60*time.Second {
						log.Printf("📍 Starting sync from configured checkpoint: %s", checkpointID)
						lastCheckpointLog = time.Now()
					}
				} else if savedHeight, savedHash, ok := loadSavedTip(m.config.Network); ok {
					_ = p2pClient.SetStartCheckpoint(savedHash, savedHeight)
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

				var tipHeader *BlockHeader
				var errTip error
				const tipRetries = 3
				backoffSecs := []int{5, 15, 30} // exponential backoff between retries (longer to avoid rate limiting)
				for attempt := 0; attempt < tipRetries; attempt++ {
					tipHeader, errTip = p2pClient.GetTipHeader()
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
				// If chain didn't link (reorg or invalid tip), handle based on whether user set explicit checkpoint
				if strings.Contains(errTip.Error(), "non-continuous headers") || strings.Contains(errTip.Error(), "does not link") {
					// Peer may be on wrong chain or sent bad headers - disconnect so we try another peer
					p2pClient.Disconnect()
					
					m.configMutex.RLock()
					userCheckpoint := m.config.P2PCheckpoint
					m.configMutex.RUnlock()
					
					if userCheckpoint != "" {
						// User explicitly selected a checkpoint - keep using it (don't fall back to genesis)
						log.Printf("⚠️ Chain validation failed - disconnecting peer, will retry from checkpoint '%s'", userCheckpoint)
						// Clear saved tip but keep the configured checkpoint
						clearSavedTip(m.config.Network)
						_ = p2pClient.SetStartCheckpointByID(userCheckpoint)
					} else {
						// No explicit checkpoint - clear everything and start from genesis
						clearSavedTip(m.config.Network)
						_ = p2pClient.SetStartCheckpoint("", 0)
						log.Printf("⚠️ Chain validation failed - disconnecting peer, starting fresh from genesis")
					}
				}
				break
				}
				if errTip != nil {
					time.Sleep(10 * time.Second)
					continue
				}
				// Save tip in display order so P2P getheaders locator is correct on resume
				hashDisplay := make([]byte, 32)
				copy(hashDisplay, tipHeader.Hash[:])
				reverseBytesInPlace(hashDisplay)
				saveTip(tipHeader.Height, hex.EncodeToString(hashDisplay), m.config.Network)

				template, err = BuildTemplateFromHeader(tipHeader, payoutAddr)
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
					template, err = BuildTemplateFromHeader(tipHeader, payoutAddr)
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
		// On testnet P2P: try a lot of nonces per template; GPU uses random start nonce so we don't always search 0..N only.
		if mode == "p2p" && network == "testnet" {
			const testnetHashesPerRound = 500000 // 500k nonces per template; random start = we cover different parts of 2^32 each round
			m.miner.MaxHashesThisRound = testnetHashesPerRound
			m.gpuMiner.MaxHashesThisRound = testnetHashesPerRound
			if time.Since(lastCheckpointLog) > 60*time.Second || lastCheckpointLog.IsZero() {
				log.Printf("🔄 Testnet: %d hashes/round, random nonce start (avoids always trying 0..N only)", testnetHashesPerRound)
			}
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
				// Throttle: log at most every 30s or every 20 rounds to avoid spam on testnet
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
				}
				lastLoggedBlockHeight = template.Height
			}
			nonce, hash, found, hashesAttempted = m.miner.MineBlock(block, template.Target)
		}
		if mode == "p2p" && network == "testnet" {
			m.miner.MaxHashesThisRound = 0
			m.gpuMiner.MaxHashesThisRound = 0
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
				m.stats.HashRate = float64(hashesAttempted) / m.stats.BlockAttemptSeconds
				m.stats.LastUpdate = time.Now()
				m.statsMutex.Unlock()
				hashCounter = 0 // already applied to TotalHashes above; avoid double-count when ticker runs
				// DON'T broadcast here - it's called multiple times per second and kills performance!
				// The 2-second ticker will broadcast automatically
			}
			
			// Validate block found result - must have valid nonce and hash (use same LE comparison as miners)
			if found && nonce != 0 && len(hash) > 0 {
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
			
			if found {
				// Only log important events (block found); hash in display order (matches block explorers)
				log.Printf("🎉 BLOCK FOUND! Hash: %s, Nonce: %d (after %d hashes)", HashToDisplayHex(hash), nonce, hashesAttempted)
				m.broadcastLogMessage("🎉 BLOCK FOUND! Check logs for details.")
				
				// CRITICAL: Check if block is stale before submitting
				// In P2P mode, verify the tip hasn't changed while we were mining
				isStale := false
				if mode == "p2p" && p2pClient != nil {
					currentTip, err := p2pClient.GetTipHeader()
					if err == nil && currentTip != nil {
						// Compare our block's previous hash to current tip hash
						expectedPrevDisplay := make([]byte, 32)
						copy(expectedPrevDisplay, currentTip.Hash[:])
						reverseBytesInPlace(expectedPrevDisplay)
						expectedPrevHex := hex.EncodeToString(expectedPrevDisplay)
						
						if expectedPrevHex != template.PreviousBlockHash {
							isStale = true
							log.Printf("⚠️ STALE BLOCK DETECTED - tip moved from height %d to %d while mining", template.Height-1, currentTip.Height)
							log.Printf("   Block builds on: %s", template.PreviousBlockHash[:16]+"...")
							log.Printf("   Current tip is:  %s", expectedPrevHex[:16]+"...")
							log.Printf("   Discarding stale block - fetching new template")
							m.broadcastLogMessage(fmt.Sprintf("⚠️ Stale block (height %d) - network moved to %d", template.Height, currentTip.Height+1))
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
				
				// Only submit if block is NOT stale
				if isStale {
					m.statsMutex.Lock()
					m.stats.BlocksStale++
					m.statsMutex.Unlock()
					
					// Record stale block
					m.submittedMutex.Lock()
					m.submittedBlocks = append(m.submittedBlocks, submittedBlock)
					if len(m.submittedBlocks) > 100 {
						m.submittedBlocks = m.submittedBlocks[len(m.submittedBlocks)-100:]
					}
					m.submittedMutex.Unlock()
				} else {
					// Attempt submission
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
						
						successMsg := "✅ Block broadcast to P2P network (acceptance not confirmed)."
						log.Println(successMsg)
						m.broadcastLogMessage(successMsg)
						if network == "testnet" {
							log.Printf("   Testnet block height %d — check https://sochain.com/block/DOGETEST/%d", template.Height, template.Height)
							log.Printf("   If that block shows another miner's address, our block was orphaned (network accepted their block first); only the accepted block gets the reward.")
						} else {
							log.Printf("   Mainnet block height %d — check your payout address after the block is confirmed (≈1 block).", template.Height)
						}
					}
					
					// Record submitted block
					m.submittedMutex.Lock()
					m.submittedBlocks = append(m.submittedBlocks, submittedBlock)
					if len(m.submittedBlocks) > 100 {
						m.submittedBlocks = m.submittedBlocks[len(m.submittedBlocks)-100:]
					}
					m.submittedMutex.Unlock()
					
					// After block broadcast, add cooling period to avoid rate limiting by peers
					if mode == "p2p" && submitErr == nil {
						cooldownSecs := 30
						if network == "testnet" {
							cooldownSecs = 20 // testnet: shorter cooldown
						}
						log.Printf("⏳ Cooling period: waiting %ds after block broadcast to avoid peer rate limiting...", cooldownSecs)
						time.Sleep(time.Duration(cooldownSecs) * time.Second)
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
	coinbaseScript := make([]byte, 0)
	// Add height as varint
	height := template.Height
	for height >= 0x80 {
		coinbaseScript = append(coinbaseScript, byte(height&0x7F)|0x80)
		height >>= 7
	}
	coinbaseScript = append(coinbaseScript, byte(height))
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
	coinbaseTx := &Transaction{
		Version:  int32(template.Version),
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
	
	// Log coinbase details for verification (only once per block)
	if logPayoutInfo {
		log.Printf("💰 Coinbase value: %d DOGE, Payout script length: %d bytes", 
			template.CoinbaseValue, len(payoutScript))
	}

	// Create block with ONLY coinbase transaction (empty block - no mempool transactions)
	// This is intentional - we only mine empty blocks to maximize hash attempts
	// We explicitly ignore template.Transactions to avoid wasting time on mempool data
	block := &Block{
		Version:       int32(template.Version),
		PrevBlock:     prevBlock,
		MerkleRoot:    merkleRoot,
		Timestamp:     uint32(template.CurTime),
		Bits:          bits,
		Nonce:         0,
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
	hash := m.miner.scryptHash(header)
	targetBig := BitsToTarget(block.Bits)
	if targetBig == nil || targetBig.Sign() <= 0 {
		return HashToDisplayHex(hash), false, fmt.Errorf("invalid bits in block")
	}
	targetBigBytes := targetBig.Bytes()
	targetBytesLE := make([]byte, 32)
	copy(targetBytesLE[32-len(targetBigBytes):], targetBigBytes)
	reverseBytesInPlace(targetBytesLE)
	meetsTarget = hashMeetsTargetLE(hash, targetBytesLE)
	return HashToDisplayHex(hash), meetsTarget, nil
}

func (m *Miner) submitBlock(block *Block, nonce uint32) error {
	// Create a copy of the block to avoid modifying the original
	blockCopy := *block
	blockCopy.Nonce = nonce
	
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
		// Re-verify PoW before broadcast so we never send an invalid block; log diagnostics for debugging
		powHashDisplay, meets, verr := m.verifyBlockPoW(&blockCopy)
		if verr != nil {
			return fmt.Errorf("block PoW verification failed: %v", verr)
		}
		if !meets {
			return fmt.Errorf("block PoW does not meet target (hash %s) — internal check failed, not sending", powHashDisplay)
		}
		prevDisplay := HashToDisplayHex(blockCopy.PrevBlock[:])
		log.Printf("📤 P2P submit: prevBlock=%s powHash=%s nonce=%d", prevDisplay, powHashDisplay, nonce)
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

	log.Printf("✅ Block submitted successfully (mode=%s). Reward will be sent to configured payout address", mode)
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
	
	// Get network and address from config
	m.configMutex.RLock()
	stats.Network = m.config.Network
	stats.PayoutAddress = m.config.PayoutAddr
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
	if err := conn.WriteJSON(stats); err != nil {
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
				RPCURL: *rpcURL,
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
					return "38150909" // biggest testnet checkpoint (Block 38,150,909) when defaulting to testnet
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
				stopChan:        make(chan struct{}),
				clients:         make(map[*websocket.Conn]bool),
				miner:           NewScryptMinerWithConfig(config.MaxHashes, config.ThreadCount),
				gpuMiner:        NewGPUMiner(config.MaxHashes, config.ThreadCount),
				enableHashLogging: true,
			}
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
				log.Println("Stopping miner to apply new configuration...")
				miner.Stop()
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
				log.Println("Restarting miner with new configuration...")
				go func() {
					// Small delay to ensure clean shutdown
					time.Sleep(500 * time.Millisecond)
					if err := miner.Start(); err != nil {
						log.Printf("❌ Failed to restart miner: %v", err)
						miner.broadcastLogMessage(fmt.Sprintf("❌ Failed to restart miner: %v", err))
					} else {
						log.Println("✅ Miner restarted with new configuration")
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
