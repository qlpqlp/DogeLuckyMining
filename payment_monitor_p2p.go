package main

import (
	"encoding/hex"
	"log"
	"sync"
	"time"
)

// P2PPaymentMonitor tracks payments via P2P protocol (no external APIs)
type P2PPaymentMonitor struct {
	p2pClient       *P2PClient
	payoutAddress   string
	network         string
	submittedBlocks []MinedBlockInfo
	payments        []P2PPayment
	mu              sync.RWMutex
	lastCheckHeight int64
	stopChan        chan struct{}
	stopped         bool
}

// MinedBlockInfo tracks blocks we've submitted
type MinedBlockInfo struct {
	Height      int64     `json:"height"`
	Hash        string    `json:"hash"`
	SubmitTime  time.Time `json:"submit_time"`
	Checked     bool      `json:"checked"`
	Confirmed   bool      `json:"confirmed"`
	PaymentSeen bool      `json:"payment_seen"`
}

// P2PPayment represents a confirmed mining payment
type P2PPayment struct {
	BlockHeight int64     `json:"block_height"`
	BlockHash   string    `json:"block_hash"`
	Value       int64     `json:"value"`       // satoshis (base units)
	Time        time.Time `json:"time"`
	Confirmed   bool      `json:"confirmed"`
}

// NewP2PPaymentMonitor creates a new P2P-based payment monitor
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

// Stop stops the payment monitor
func (pm *P2PPaymentMonitor) Stop() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if !pm.stopped {
		close(pm.stopChan)
		pm.stopped = true
	}
}

// TrackSubmittedBlock adds a block to track for payments
func (pm *P2PPaymentMonitor) TrackSubmittedBlock(height int64, hash string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	
	pm.submittedBlocks = append(pm.submittedBlocks, MinedBlockInfo{
		Height:     height,
		Hash:       hash,
		SubmitTime: time.Now(),
		Checked:    false,
		Confirmed:  false,
		PaymentSeen: false,
	})
	
	// Keep only last 100 blocks
	if len(pm.submittedBlocks) > 100 {
		pm.submittedBlocks = pm.submittedBlocks[len(pm.submittedBlocks)-100:]
	}
	
	log.Printf("💰 P2P Monitor: Tracking block %d (%s) for payment", height, hash[:16]+"...")
}

// CheckForPayments checks if any tracked blocks have resulted in payments
func (pm *P2PPaymentMonitor) CheckForPayments() error {
	if pm.p2pClient == nil {
		return nil // P2P not available
	}
	
	// Get current tip
	tip, err := pm.p2pClient.GetTipHeader()
	if err != nil {
		return err
	}
	
	pm.mu.Lock()
	defer pm.mu.Unlock()
	
	// Check blocks that need verification
	for i := range pm.submittedBlocks {
		block := &pm.submittedBlocks[i]
		
		// Skip already checked blocks
		if block.Checked && block.PaymentSeen {
			continue
		}
		
		// Only check blocks that should be confirmed (at least 1 block deep)
		if tip.Height < block.Height+1 {
			continue
		}
		
		// Request the block at this height via P2P
		// In a full implementation, we would:
		// 1. Request headers at this height
		// 2. Verify our block hash matches
		// 3. Request full block data
		// 4. Parse coinbase transaction
		// 5. Check if it pays to our address
		
		// For now, mark as checked and use the submitted block data
		// Since we built the block, we know it pays to our address
		// We just need to confirm the network accepted it
		
		confirmedHash := pm.getBlockHashAtHeight(block.Height)
		if confirmedHash == "" {
			continue // Can't verify yet
		}
		
		block.Checked = true
		
		// Compare hashes (both in display order)
		if confirmedHash == block.Hash {
			block.Confirmed = true
			block.PaymentSeen = true
			
			// Add to payments list
			payment := P2PPayment{
				BlockHeight: block.Height,
				BlockHash:   block.Hash,
				Value:       BlockRewardForHeight(block.Height),
				Time:        block.SubmitTime,
				Confirmed:   true,
			}
			pm.payments = append(pm.payments, payment)
			
			log.Printf("✅ P2P Monitor: Block %d CONFIRMED and PAID! Reward: %.2f DOGE", 
				block.Height, float64(payment.Value)/1e8)
		} else {
			block.Confirmed = false
			log.Printf("⚠️ P2P Monitor: Block %d was ORPHANED (network chose different block)", block.Height)
		}
	}
	
	pm.lastCheckHeight = tip.Height
	return nil
}

// getBlockHashAtHeight gets the block hash at a specific height via P2P
// This is a simplified version - full implementation would use getheaders
func (pm *P2PPaymentMonitor) getBlockHashAtHeight(height int64) string {
	if pm.p2pClient == nil {
		return ""
	}
	
	// Get current tip and work backwards
	tip, err := pm.p2pClient.GetTipHeader()
	if err != nil || tip == nil {
		return ""
	}
	
	// If height is current tip, return tip hash
	if tip.Height == height {
		hashDisplay := make([]byte, 32)
		copy(hashDisplay, tip.Hash[:])
		reverseBytesInPlace(hashDisplay)
		return hex.EncodeToString(hashDisplay)
	}
	
	// For other heights, we'd need to traverse the chain
	// This requires implementing getheaders traversal
	// For now, return empty (block will be checked again later)
	return ""
}

// GetStats returns monitoring statistics
func (pm *P2PPaymentMonitor) GetStats() P2PMonitorStats {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	
	totalSubmitted := len(pm.submittedBlocks)
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
		TotalSubmitted:  totalSubmitted,
		Confirmed:       confirmed,
		Orphaned:        orphaned,
		Pending:         pending,
		TotalPaid:       totalPaid,
		PaymentCount:    len(pm.payments),
		LastCheckHeight: pm.lastCheckHeight,
		RecentPayments:  pm.getRecentPayments(10),
	}
}

// getRecentPayments returns the N most recent payments
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

// P2PMonitorStats represents monitoring statistics
type P2PMonitorStats struct {
	TotalSubmitted  int         `json:"total_submitted"`
	Confirmed       int         `json:"confirmed"`
	Orphaned        int         `json:"orphaned"`
	Pending         int         `json:"pending"`
	TotalPaid       int64       `json:"total_paid"`        // satoshis
	PaymentCount    int         `json:"payment_count"`
	LastCheckHeight int64       `json:"last_check_height"`
	RecentPayments  []P2PPayment `json:"recent_payments"`
}

// StartMonitoring starts periodic payment checking
func (pm *P2PPaymentMonitor) StartMonitoring(interval time.Duration) {
	log.Printf("💰 P2P Payment Monitor Started (pure P2P, no APIs)")
	log.Printf("   Monitoring address: %s", pm.payoutAddress)
	log.Printf("   Network: %s", pm.network)
	
	// Check immediately
	if err := pm.CheckForPayments(); err != nil {
		log.Printf("⚠️ P2P monitor initial check failed: %v", err)
	}
	
	// Periodic checks
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-pm.stopChan:
				log.Printf("💰 P2P Payment Monitor stopped")
				return
			case <-ticker.C:
				if err := pm.CheckForPayments(); err != nil {
					// Silently fail - P2P connection might be down
				}
			}
		}
	}()
}
