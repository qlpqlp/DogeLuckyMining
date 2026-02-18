package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	mathrand "math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/scrypt"
)

type ScryptMiner struct {
	// Scrypt parameters for Dogecoin
	N int // CPU/memory cost parameter (1024 for Dogecoin)
	R int // Block size parameter (1 for Dogecoin)
	P int // Parallelization parameter (1 for Dogecoin)
	// Mining performance settings
	MaxHashes         uint64 // Maximum hashes to attempt per block template
	MaxHashesThisRound uint64 // If > 0, cap at this many hashes this call (for testnet fast refresh)
	ThreadCount       int    // Number of parallel mining threads
	// Nonce search strategy settings
	NonceSearchMode string // "forward", "backward", "sequential" (default)
	NonceStartMode  string // "random", "calculated" (from block metadata)
	UseStride       bool   // Use stride-based iteration for multi-threading
}

type MiningResult struct {
	Nonce      uint32
	Hash       []byte
	Found      bool
	HashCount  uint64 // Number of hashes attempted
}

func NewScryptMiner() *ScryptMiner {
	// Dogecoin uses Scrypt with N=1024, r=1, p=1
	return &ScryptMiner{
		N:         1024,
		R:         1,
		P:         1,
		MaxHashes: 100000, // Default max hashes per block
		ThreadCount: 1,    // Default single thread
	}
}

func NewScryptMinerWithConfig(maxHashes uint64, threadCount int) *ScryptMiner {
	return &ScryptMiner{
		N:          1024,
		R:          1,
		P:          1,
		MaxHashes:  maxHashes,
		ThreadCount: threadCount,
		NonceSearchMode: "sequential", // Default: sequential from 0
		NonceStartMode:  "calculated", // Default: calculate from block metadata
		UseStride:       false,        // Default: no stride (range-based)
	}
}

// SetNonceStrategy configures nonce search strategy
func (m *ScryptMiner) SetNonceStrategy(searchMode, startMode string, useStride bool) {
	m.NonceSearchMode = searchMode
	m.NonceStartMode = startMode
	m.UseStride = useStride
}

// MineBlock attempts to mine a block using predictive/random nonce selection
// Returns: nonce, hash, found status, and number of hashes attempted
func (m *ScryptMiner) MineBlock(block *Block, targetHex string) (uint32, []byte, bool, uint64) {
	// Convert target from hex string to big.Int
	targetBytes, err := hex.DecodeString(targetHex)
	if err != nil {
		// If target is not hex, return false
		return 0, nil, false, 0
	}

	// Target is already in big-endian format from RPC
	targetBig := new(big.Int).SetBytes(targetBytes)
	if targetBig.Cmp(big.NewInt(0)) == 0 {
		targetBig = big.NewInt(1)
	}

	// Use parallel mining if thread count > 1, otherwise use single-threaded strategy
	if m.ThreadCount > 1 {
		return m.mineParallel(block, targetBig)
	}
	
	// Use predictive/random mining strategy (single-threaded)
	return m.mineWithStrategy(block, targetBig)
}

func (m *ScryptMiner) effectiveMaxHashes() uint64 {
	limit := m.MaxHashes
	if m.MaxHashesThisRound > 0 && m.MaxHashesThisRound < limit {
		limit = m.MaxHashesThisRound
	}
	return limit
}

// mineWithStrategy uses multiple strategies to find a valid nonce
// OPTIMIZED: Pre-computes target bytes and reuses buffers
func (m *ScryptMiner) mineWithStrategy(block *Block, targetBig *big.Int) (uint32, []byte, bool, uint64) {
	hashCount := uint64(0)
	maxHashes := m.effectiveMaxHashes()
	
	// Pre-compute target in LE (Dogecoin Core uses uint256 LE for PoW comparison)
	targetBytes := targetBig.Bytes()
	targetBytesLE := make([]byte, 32)
	copy(targetBytesLE[32-len(targetBytes):], targetBytes)
	reverseBytesInPlace(targetBytesLE)
	
	// Pre-compute header base (reuse for all nonces)
	headerBase := m.precomputeHeaderBase(block)
	headerBuf := make([]byte, len(headerBase)+4)
	copy(headerBuf, headerBase)
	
	// Initialize math random with time seed for additional randomness
	rng := mathrand.New(mathrand.NewSource(time.Now().UnixNano()))
	
	// Strategy 1: Random nonce selection (most likely to find blocks first)
	// Try random nonces across the entire range
	randomNonces := make([]uint32, 1000)
	for i := range randomNonces {
		randomNonces[i] = rng.Uint32()
	}
	
	// Try random nonces first (lucky guesses)
	for _, nonce := range randomNonces {
		if hashCount >= maxHashes {
			break
		}
		
		// Update nonce in pre-allocated buffer
		binary.LittleEndian.PutUint32(headerBuf[len(headerBase):], nonce)
		hash := m.scryptHash(headerBuf)
		hashCount++
		
		if hashMeetsTargetLE(hash, targetBytesLE) {
			return nonce, hash, true, hashCount
		}
	}
	
	// Strategy 2: Pattern-based nonce selection
	// Try nonces at specific intervals that might be "lucky"
	patterns := []uint32{
		0x00000000, 0xFFFFFFFF, 0x12345678, 0xABCDEF00,
		0x11111111, 0x22222222, 0x33333333, 0x44444444,
		0x55555555, 0x66666666, 0x77777777, 0x88888888,
		0x99999999, 0xAAAAAAAA, 0xBBBBBBBB, 0xCCCCCCCC,
		0xDDDDDDDD, 0xEEEEEEEE, 0xFEDCBA98, 0x76543210,
	}
	
	for _, nonce := range patterns {
		if hashCount >= maxHashes {
			break
		}
		
		// Update nonce in pre-allocated buffer
		binary.LittleEndian.PutUint32(headerBuf[len(headerBase):], nonce)
		hash := m.scryptHash(headerBuf)
		hashCount++
		
		if hashMeetsTargetLE(hash, targetBytesLE) {
			return nonce, hash, true, hashCount
		}
	}
	
	// Strategy 3: Sequential with random starting point
	startNonce := rng.Uint32()
	
	// Try sequential nonces from random start
	for i := uint32(0); i < 50000 && hashCount < maxHashes; i++ {
		nonce := (startNonce + i) % 0xFFFFFFFF
		
		// Update nonce in pre-allocated buffer
		binary.LittleEndian.PutUint32(headerBuf[len(headerBase):], nonce)
		hash := m.scryptHash(headerBuf)
		hashCount++
		
		if hashMeetsTargetLE(hash, targetBytesLE) {
			return nonce, hash, true, hashCount
		}
	}
	
	// Strategy 4: Bit-flipping strategy (try nonces with specific bit patterns)
	// Try nonces with leading zeros/ones which might produce better hashes
	bitPatterns := []struct {
		mask  uint32
		value uint32
	}{
		{0xFF000000, 0x00000000}, // Leading zeros
		{0xFF000000, 0xFF000000}, // Leading ones
		{0x00FF0000, 0x0000FF00}, // Middle patterns
		{0x0000FFFF, 0x0000FFFF}, // Trailing patterns
	}
	
	for _, pattern := range bitPatterns {
		if hashCount >= maxHashes {
			break
		}
		
		// Generate nonces matching the pattern
		for j := uint32(0); j < 1000 && hashCount < maxHashes; j++ {
			nonce := (pattern.value & pattern.mask) | (j &^ pattern.mask)
			
			// Update nonce in pre-allocated buffer
			binary.LittleEndian.PutUint32(headerBuf[len(headerBase):], nonce)
			hash := m.scryptHash(headerBuf)
			hashCount++
			
			if hashMeetsTargetLE(hash, targetBytesLE) {
				return nonce, hash, true, hashCount
			}
		}
	}
	
	// Strategy 5: Prime number intervals (sometimes patterns emerge)
	primes := []uint32{2, 3, 5, 7, 11, 13, 17, 19, 23, 29, 31, 37, 41, 43, 47, 53, 59, 61, 67, 71, 73, 79, 83, 89, 97}
	baseNonce := rng.Uint32() & 0xFFFF // Use lower 16 bits for base
	
	for _, prime := range primes {
		if hashCount >= maxHashes {
			break
		}
		
		for mult := uint32(0); mult < 1000 && hashCount < maxHashes; mult++ {
			nonce := (baseNonce + prime*mult) % 0xFFFFFFFF
			
			// Update nonce in pre-allocated buffer
			binary.LittleEndian.PutUint32(headerBuf[len(headerBase):], nonce)
			hash := m.scryptHash(headerBuf)
			hashCount++
			
			if hashMeetsTargetLE(hash, targetBytesLE) {
				return nonce, hash, true, hashCount
			}
		}
	}
	
	return 0, nil, false, hashCount
}

// calculateStartNonce computes starting nonce from block metadata (same as GPU miner)
func calculateStartNonceCPU(block *Block, startMode string) uint32 {
	if startMode == "random" {
		// Use current time as seed for randomness
		return uint32(time.Now().UnixNano() & 0xFFFFFFFF)
	}
	
	// "calculated" mode: Use block metadata (Version XOR Timestamp XOR Bits)
	startNonce := uint32(block.Version) ^ block.Timestamp ^ block.Bits
	
	// Also XOR with merkle root bytes for more entropy
	for i := 0; i < 4 && i < len(block.MerkleRoot); i++ {
		startNonce ^= uint32(block.MerkleRoot[i]) << (i * 8)
	}
	
	return startNonce
}

// mineParallel uses multiple goroutines to mine in parallel (for CPU mode with multiple threads)
func (m *ScryptMiner) mineParallel(block *Block, targetBig *big.Int) (uint32, []byte, bool, uint64) {
	var foundNonce uint32
	var foundHash []byte
	var found atomic.Bool
	var totalHashes atomic.Uint64

	// Pre-compute static header parts (everything except nonce) for performance
	headerBase := m.precomputeHeaderBase(block)

	// Calculate hashes per thread
	hashesPerThread := m.effectiveMaxHashes() / uint64(m.ThreadCount)
	if hashesPerThread == 0 {
		hashesPerThread = 1000 // Minimum per thread
	}

	var wg sync.WaitGroup
	results := make(chan struct {
		nonce uint32
		hash  []byte
	}, m.ThreadCount)

	// Calculate starting nonce based on strategy
	startNonceBase := calculateStartNonceCPU(block, m.NonceStartMode)
	
	// Determine nonce distribution strategy
	var nonceRangePerThread uint64
	if m.UseStride {
		// Stride-based: each thread uses stride = threadCount, starting from startNonceBase + threadID
		nonceRangePerThread = 0 // Not used in stride mode
	} else {
		// Range-based: divide nonce space into ranges
		nonceRangePerThread = uint64(0xFFFFFFFF) / uint64(m.ThreadCount)
	}

	// Start parallel mining threads with optimized deterministic nonce ranges
	for t := 0; t < m.ThreadCount; t++ {
		wg.Add(1)
		go func(threadID int) {
			defer wg.Done()

			// Pin goroutine to CPU core for better cache locality
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			localHashCount := uint64(0)
			maxLocalHashes := hashesPerThread

			// Pre-allocate buffers to reduce allocations
			headerBuf := make([]byte, len(headerBase)+4)
			copy(headerBuf, headerBase)
			
			// Pre-compute target in LE for PoW comparison
			targetBytes := targetBig.Bytes()
			targetBytesLE := make([]byte, 32)
			copy(targetBytesLE[32-len(targetBytes):], targetBytes)
			reverseBytesInPlace(targetBytesLE)
			
			// Determine nonce iteration strategy
			var threadStartNonce, threadEndNonce uint32
			var stride uint32
			
			if m.UseStride {
				// Stride-based: each thread uses stride = threadCount
				stride = uint32(m.ThreadCount)
				threadStartNonce = (startNonceBase + uint32(threadID)) % 0xFFFFFFFF
				threadEndNonce = 0xFFFFFFFF // No end limit, wraps around
			} else {
				// Range-based: divide nonce space into ranges
				threadStartNonce = startNonceBase + uint32(uint64(threadID)*nonceRangePerThread)
				threadEndNonce = startNonceBase + uint32(uint64(threadID+1)*nonceRangePerThread)
				if threadID == m.ThreadCount-1 {
					threadEndNonce = 0xFFFFFFFF
				}
			}

			// Process nonces based on search mode
			currentNonce := threadStartNonce
			batchSize := 1000
			
			if m.NonceSearchMode == "backward" {
				// Backward search: count down from startNonce
				for localHashCount < maxLocalHashes && currentNonce > 0 && !found.Load() {
					batchEnd := currentNonce
					if currentNonce > uint32(batchSize) {
						batchEnd = currentNonce - uint32(batchSize)
					} else {
						batchEnd = 0
					}
					
					for nonce := currentNonce; nonce > batchEnd && localHashCount < maxLocalHashes && !found.Load(); nonce-- {
						binary.LittleEndian.PutUint32(headerBuf[len(headerBase):], nonce)
						hash := m.scryptHash(headerBuf)
						localHashCount++

						if hashMeetsTargetLE(hash, targetBytesLE) {
							if !found.Swap(true) {
								results <- struct {
									nonce uint32
									hash  []byte
								}{nonce, hash}
							}
							return
						}
					}
					
					currentNonce = batchEnd
					if currentNonce == 0 && localHashCount < maxLocalHashes {
						currentNonce = 0xFFFFFFFF // Wrap around
					}
				}
			} else {
				// Forward or sequential search
				for localHashCount < maxLocalHashes && !found.Load() {
					var batchEnd uint32
					
					if m.UseStride {
						// Stride-based iteration: nonce = startNonce + i*stride
						// Process stride-based batch
						for i := uint32(0); i < uint32(batchSize) && localHashCount < maxLocalHashes && !found.Load(); i++ {
							nonce := (threadStartNonce + i*stride) % 0xFFFFFFFF
							binary.LittleEndian.PutUint32(headerBuf[len(headerBase):], nonce)
							hash := m.scryptHash(headerBuf)
							localHashCount++

							if hashMeetsTargetLE(hash, targetBytesLE) {
								if !found.Swap(true) {
									results <- struct {
										nonce uint32
										hash  []byte
									}{nonce, hash}
								}
								return
							}
						}
						// Update for next stride batch
						threadStartNonce = (threadStartNonce + uint32(batchSize)*stride) % 0xFFFFFFFF
						continue
					} else {
						// Sequential range-based
						batchEnd = currentNonce + uint32(batchSize)
						if batchEnd > threadEndNonce {
							batchEnd = threadEndNonce
						}
						
						for nonce := currentNonce; nonce < batchEnd && localHashCount < maxLocalHashes && !found.Load(); nonce++ {
							binary.LittleEndian.PutUint32(headerBuf[len(headerBase):], nonce)
							hash := m.scryptHash(headerBuf)
							localHashCount++

							if hashMeetsTargetLE(hash, targetBytesLE) {
								if !found.Swap(true) {
									results <- struct {
										nonce uint32
										hash  []byte
									}{nonce, hash}
								}
								return
							}
						}
						
						currentNonce = batchEnd
						
						// Wrap around if exhausted range
						if currentNonce >= threadEndNonce && localHashCount < maxLocalHashes {
							currentNonce = threadStartNonce
						}
					}
				}
			}

			totalHashes.Add(localHashCount)
		}(t)
	}

	// Wait for first result or all threads to complete
	done := make(chan bool, 1)
	go func() {
		wg.Wait()
		close(results)
		done <- true
	}()

	// Try to get a result first
	select {
	case result, ok := <-results:
		if ok && result.nonce != 0 && len(result.hash) > 0 {
			foundNonce = result.nonce
			foundHash = result.hash
			found.Store(true)
			wg.Wait()
			// Double-check the hash meets the target (fast comparison)
			targetBytes := targetBig.Bytes()
			targetBytesLE := make([]byte, 32)
			copy(targetBytesLE[32-len(targetBytes):], targetBytes)
			reverseBytesInPlace(targetBytesLE)
			if hashMeetsTargetLE(foundHash, targetBytesLE) {
				return foundNonce, foundHash, true, totalHashes.Load()
			}
		}
		<-done
		return 0, nil, false, totalHashes.Load()
	case <-done:
		return 0, nil, false, totalHashes.Load()
	}
}


// precomputeHeaderBase pre-computes the static part of the block header (without nonce)
func (m *ScryptMiner) precomputeHeaderBase(block *Block) []byte {
	blockCopy := *block
	blockCopy.Nonce = 0
	header := blockCopy.SerializeHeader()
	return header[:len(header)-4]
}

// scryptHash computes Dogecoin/Litecoin PoW: scrypt(header80, header80, N=1024, r=1, p=1) -> 32 bytes.
// Same as cpuminer-multi (tpruvot) and cgminer (ozbenh): 80-byte block header as password and salt.
// PBKDF2-HMAC uses the 80-byte key; HMAC-SHA256 hashes keys >64 bytes, so effective key is SHA256(header).
// Header must be in wire order (version LE, prevBlock, merkle, time LE, bits LE, nonce LE).
func (m *ScryptMiner) scryptHash(header80 []byte) []byte {
	if len(header80) != 80 {
		panic(fmt.Sprintf("scryptHash requires 80-byte header, got %d", len(header80)))
	}
	hash, err := scrypt.Key(header80, header80, m.N, m.R, m.P, 32)
	if err != nil {
		panic(fmt.Sprintf("Scrypt error: %v", err))
	}
	return hash
}

func reverseBytes(b []byte) []byte {
	result := make([]byte, len(b))
	for i := 0; i < len(b); i++ {
		result[i] = b[len(b)-1-i]
	}
	return result
}
