package main

import (
	"log"
	"sync"
	"time"
)

// P2PPaymentMonitor tracks whether submitted blocks were accepted by the network
// using ONLY data from the P2P header stream — no external APIs, no RPC.
//
// Verification strategy (pure P2P):
//
//	As the mining loop processes incoming block headers via GetTipHeader(), the
//	P2PClient.tipHistory map records the display-order hash of every confirmed
//	block it sees (populated from each header's hash computed during chain sync).
//	When enough confirmations have passed for a submitted block, the monitor
//	queries tipHistory[submittedHeight] and compares it to our block hash.
//	Match → confirmed & paid; mismatch → orphaned (another miner's block won).
type P2PPaymentMonitor struct {
	p2pClient     *P2PClient // shared reference to the MAIN miner P2P client (read-only)
	payoutAddress string
	network       string

	submittedBlocks []MinedBlockInfo
	payments        []P2PPayment

	mu              sync.RWMutex
	lastCheckHeight int64
	stopChan        chan struct{}
	stopped         bool
}

// MinedBlockInfo tracks a block we've submitted.
type MinedBlockInfo struct {
	Height      int64     `json:"height"`
	Hash        string    `json:"hash"` // display order (hex, as used in block explorers)
	SubmitTime  time.Time `json:"submit_time"`
	Checked     bool      `json:"checked"`
	Confirmed   bool      `json:"confirmed"`
	PaymentSeen bool      `json:"payment_seen"`
}

// P2PPayment represents a confirmed mining payment.
type P2PPayment struct {
	BlockHeight int64     `json:"block_height"`
	BlockHash   string    `json:"block_hash"`
	Value       int64     `json:"value"` // koinu (base units)
	Time        time.Time `json:"time"`
	Confirmed   bool      `json:"confirmed"`
}

// NewP2PPaymentMonitor creates a new payment monitor.
// p2pClient must be the MAIN miner P2P client — the monitor reads its tipHistory
// which is kept up-to-date by the mining loop; no extra network connection is created.
func NewP2PPaymentMonitor(p2pClient *P2PClient, payoutAddress, network string) *P2PPaymentMonitor {
	return &P2PPaymentMonitor{
		p2pClient:       p2pClient,
		payoutAddress:   payoutAddress,
		network:         network,
		submittedBlocks: make([]MinedBlockInfo, 0),
		payments:        make([]P2PPayment, 0),
		lastCheckHeight: 0,
		stopChan:        make(chan struct{}),
		stopped:         false,
	}
}

// Stop stops the payment monitor.
func (pm *P2PPaymentMonitor) Stop() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if !pm.stopped {
		close(pm.stopChan)
		pm.stopped = true
	}
}

// TrackSubmittedBlock registers a block for payment confirmation tracking.
func (pm *P2PPaymentMonitor) TrackSubmittedBlock(height int64, hash string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.submittedBlocks = append(pm.submittedBlocks, MinedBlockInfo{
		Height:      height,
		Hash:        hash,
		SubmitTime:  time.Now(),
		Checked:     false,
		Confirmed:   false,
		PaymentSeen: false,
	})

	// Keep only last 100 blocks
	if len(pm.submittedBlocks) > 100 {
		pm.submittedBlocks = pm.submittedBlocks[len(pm.submittedBlocks)-100:]
	}

	abbrev := hash
	if len(abbrev) > 16 {
		abbrev = abbrev[:16]
	}
	log.Printf("💰 Payment Monitor: Tracking block %d (%s...) for P2P confirmation", height, abbrev)
}

// CheckForPayments inspects the P2P tip history to confirm or orphan submitted blocks.
// It requires at least 2 confirmations (tip.Height >= block.Height + 2) before deciding,
// so there is time for the tipHistory to record the hash at the submitted height.
func (pm *P2PPaymentMonitor) CheckForPayments() error {
	if pm.p2pClient == nil {
		return nil
	}

	// Read the current chain tip height from the cached (non-blocking) value.
	tip := pm.p2pClient.GetCachedTip()
	if tip == nil {
		return nil // Not yet synced; will retry on next tick
	}
	tipHeight := tip.Height

	pm.mu.Lock()
	defer pm.mu.Unlock()

	for i := range pm.submittedBlocks {
		block := &pm.submittedBlocks[i]

		if block.Checked {
			continue // Already resolved
		}

		// Require at least 2 blocks of confirmation depth before checking.
		// This gives the tipHistory time to record the hash at block.Height.
		if tipHeight < block.Height+2 {
			continue
		}

		// Query the P2P tip history for the hash the network recorded at our height.
		networkHash := pm.p2pClient.GetBlockHashAtHeight(block.Height)
		if networkHash == "" {
			// Not yet in history (e.g. the checkpoint was set above this height and we
			// never processed that header). Log a one-time warning and leave pending.
			log.Printf("💰 Payment Monitor: Block %d not yet in P2P header history (tip=%d); will retry", block.Height, tipHeight)
			continue
		}

		block.Checked = true

		if networkHash == block.Hash {
			// Our block is in the main chain — payment confirmed!
			block.Confirmed = true
			block.PaymentSeen = true

			payment := P2PPayment{
				BlockHeight: block.Height,
				BlockHash:   block.Hash,
				Value:       BlockRewardForHeight(block.Height),
				Time:        block.SubmitTime,
				Confirmed:   true,
			}
			pm.payments = append(pm.payments, payment)

			log.Printf("✅ Payment Monitor: Block %d CONFIRMED on P2P chain! Reward: %.2f DOGE → %s",
				block.Height, float64(payment.Value)/1e8, pm.payoutAddress)
		} else {
			block.Confirmed = false
			nb := networkHash
			ob := block.Hash
			if len(nb) > 16 {
				nb = nb[:16]
			}
			if len(ob) > 16 {
				ob = ob[:16]
			}
			log.Printf("⚠️  Payment Monitor: Block %d ORPHANED — network block is %s... but ours was %s...",
				block.Height, nb, ob)
		}
	}

	pm.lastCheckHeight = tipHeight
	return nil
}

// GetStats returns monitoring statistics.
func (pm *P2PPaymentMonitor) GetStats() P2PMonitorStats {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	confirmed := 0
	orphaned := 0
	pending := 0
	totalPaid := int64(0)

	for _, block := range pm.submittedBlocks {
		if block.Checked {
			if block.Confirmed {
				confirmed++
			} else {
				orphaned++
			}
		} else {
			pending++
		}
	}

	for _, payment := range pm.payments {
		totalPaid += payment.Value
	}

	return P2PMonitorStats{
		TotalSubmitted:  len(pm.submittedBlocks),
		Confirmed:       confirmed,
		Orphaned:        orphaned,
		Pending:         pending,
		TotalPaid:       totalPaid,
		PaymentCount:    len(pm.payments),
		LastCheckHeight: pm.lastCheckHeight,
		RecentPayments:  pm.getRecentPayments(10),
	}
}

func (pm *P2PPaymentMonitor) getRecentPayments(n int) []P2PPayment {
	if len(pm.payments) <= n {
		result := make([]P2PPayment, len(pm.payments))
		copy(result, pm.payments)
		return result
	}
	result := make([]P2PPayment, n)
	copy(result, pm.payments[len(pm.payments)-n:])
	return result
}

// P2PMonitorStats represents monitoring statistics.
type P2PMonitorStats struct {
	TotalSubmitted  int          `json:"total_submitted"`
	Confirmed       int          `json:"confirmed"`
	Orphaned        int          `json:"orphaned"`
	Pending         int          `json:"pending"`
	TotalPaid       int64        `json:"total_paid"` // koinu
	PaymentCount    int          `json:"payment_count"`
	LastCheckHeight int64        `json:"last_check_height"`
	RecentPayments  []P2PPayment `json:"recent_payments"`
}

// StartMonitoring starts periodic payment checking.
func (pm *P2PPaymentMonitor) StartMonitoring(interval time.Duration) {
	log.Printf("💰 Payment Monitor started (pure P2P header verification)")
	log.Printf("   Payout address: %s", pm.payoutAddress)
	log.Printf("   Network: %s | Check interval: %s", pm.network, interval)

	// Initial best-effort check
	if err := pm.CheckForPayments(); err != nil {
		log.Printf("Payment monitor: initial check skipped (%v); will retry", err)
	}

	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-pm.stopChan:
				log.Printf("💰 Payment Monitor stopped")
				return
			case <-ticker.C:
				_ = pm.CheckForPayments()
			}
		}
	}()
}
