package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
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
	// Debug logging
	DebugLogging    bool   // Enable verbose debug output
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
		N:            1024,
		R:            1,
		P:            1,
		MaxHashes:    100000, // Default max hashes per block
		ThreadCount:  1,      // Default single thread
		DebugLogging: false,  // Default: no debug logging
	}
}

func NewScryptMinerWithConfig(maxHashes uint64, threadCount int) *ScryptMiner {
	return &ScryptMiner{
		N:               1024,
		R:               1,
		P:               1,
		MaxHashes:       maxHashes,
		ThreadCount:     threadCount,
		NonceSearchMode: "sequential", // Default: sequential from 0
		NonceStartMode:  "calculated", // Default: calculate from block metadata
		UseStride:       false,        // Default: no stride (range-based)
		DebugLogging:    false,        // Default: no debug logging
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
	if m.DebugLogging {
		log.Printf("🔨 MINING START: Block height %d, PrevBlock hash (display): %s", block.Version, hex.EncodeToString(block.PrevBlock[:4]))
	}

	// Convert target from hex string to big.Int
	targetBytes, err := hex.DecodeString(targetHex)
	if err != nil {
		// If target is not hex, return false
		if m.DebugLogging {
			log.Printf("❌ Invalid target hex: %v", err)
		}
		return 0, nil, false, 0
	}

	// Target is already in big-endian format from RPC
	targetBig := new(big.Int).SetBytes(targetBytes)
	if targetBig.Cmp(big.NewInt(0)) == 0 {
		targetBig = big.NewInt(1)
	}

	if m.DebugLogging {
		log.Printf("📊 TARGET: %s (big-endian hex from RPC/P2P)", hex.EncodeToString(targetBytes))
		log.Printf("   → Max nonces to test: %d", m.effectiveMaxHashes())
	}

	// Use parallel mining if thread count > 1, otherwise use single-threaded strategy
	if m.ThreadCount > 1 {
		return m.mineParallel(block, targetBig)
	}
	
	// Use predictive/random mining strategy (single-threaded)
	return m.mineWithStrategy(block, targetBig)
}

func (m *ScryptMiner) effectiveMaxHashes() uint64 {
	// When MaxHashesThisRound is set (e.g. testnet 2M/round), use it so we try more nonces per template.
	if m.MaxHashesThisRound > 0 {
		return m.MaxHashesThisRound
	}
	return m.MaxHashes
}

// mineWithStrategy searches for a valid nonce using a random start + sequential sweep.
// This ensures uniform coverage of the nonce space across successive calls with different block templates.
func (m *ScryptMiner) mineWithStrategy(block *Block, targetBig *big.Int) (uint32, []byte, bool, uint64) {
	hashCount := uint64(0)
	maxHashes := m.effectiveMaxHashes()

	// Pre-compute target in LE (Dogecoin Core uses uint256 LE for PoW comparison)
	targetBytes := targetBig.Bytes()
	targetBytesLE := make([]byte, 32)
	copy(targetBytesLE[32-len(targetBytes):], targetBytes)
	reverseBytesInPlace(targetBytesLE)

	if m.DebugLogging {
		log.Printf("🔄 CPU SINGLE-THREADED MINING:")
		log.Printf("   Target (BE from RPC):   %s", hex.EncodeToString(targetBytes))
		log.Printf("   Target (LE for comparison): %s", hex.EncodeToString(targetBytesLE))
		log.Printf("   Scrypt params: N=%d, r=%d, p=%d (Dogecoin standard)", m.N, m.R, m.P)
	}

	// Pre-compute header base (reuse for all nonces)
	headerBase := m.precomputeHeaderBase(block)
	headerBuf := make([]byte, len(headerBase)+4)
	copy(headerBuf, headerBase)

	if m.DebugLogging {
		log.Printf("   Header base (76 bytes): %s", hex.EncodeToString(headerBase[:16])+"...")
	}

	// Random start so each round covers a different nonce region (critical for finding blocks)
	startNonce := mathrand.New(mathrand.NewSource(time.Now().UnixNano())).Uint32()

	if m.DebugLogging {
		log.Printf("   Starting nonce (random): 0x%08x", startNonce)
		log.Printf("   Search pattern: sequential (0x00000001, 0x00000002, ... 0xFFFFFFFF)")
	}

	for i := uint64(0); i < maxHashes; i++ {
		nonce := uint32((uint64(startNonce) + i) & 0xFFFFFFFF)
		binary.LittleEndian.PutUint32(headerBuf[len(headerBase):], nonce)
		hashLE := m.scryptHash(headerBuf) // scryptHash returns LE format directly - no reversal needed
		hashCount++

		// Educational debug: show sample hashes and comparisons
		if m.DebugLogging && (i == 0 || i == 99 || i%1000 == 0 || hashCount%10000 == 0) {
			meets := hashMeetsTargetLE(hashLE, targetBytesLE)
			log.Printf("   [%d] Nonce 0x%08x: hash=%s... meets_target=%v", hashCount, nonce, hex.EncodeToString(hashLE)[:16], meets)
		}

		if hashMeetsTargetLE(hashLE, targetBytesLE) {
			if m.DebugLogging {
				log.Printf("✅ BLOCK FOUND! Nonce 0x%08x after %d hashes", nonce, hashCount)
			}
			// Return hash in display order (reversed from LE) for logging
			hashDisplay := make([]byte, 32)
			copy(hashDisplay, hashLE)
			reverseBytesInPlace(hashDisplay)
			return nonce, hashDisplay, true, hashCount
		}
	}

	if m.DebugLogging {
		log.Printf("⚠️ No block found in %d hashes (nonces 0x%08x to 0x%08x)", maxHashes, startNonce, uint32((uint64(startNonce)+maxHashes-1)&0xFFFFFFFF))
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

	if m.DebugLogging {
		log.Printf("⚙️ CPU PARALLEL MINING:")
		log.Printf("   Threads: %d (each pins to a CPU core)", m.ThreadCount)
		log.Printf("   Strategy: %s nonce iteration, %s start", m.NonceSearchMode, m.NonceStartMode)
		if m.UseStride {
			log.Printf("   Distribution: STRIDE (each thread increments by %d)", m.ThreadCount)
		} else {
			log.Printf("   Distribution: RANGE (divide nonce space into %d ranges)", m.ThreadCount)
		}
	}

	// Pre-compute static header parts (everything except nonce) for performance
	headerBase := m.precomputeHeaderBase(block)

	// Calculate hashes per thread
	hashesPerThread := m.effectiveMaxHashes() / uint64(m.ThreadCount)
	if hashesPerThread == 0 {
		hashesPerThread = 1000 // Minimum per thread
	}

	if m.DebugLogging {
		log.Printf("   Total hashes: %d, per thread: %d", m.effectiveMaxHashes(), hashesPerThread)
	}

	var wg sync.WaitGroup
	results := make(chan struct {
		nonce uint32
		hash  []byte
	}, m.ThreadCount)

	// Always use random start so each mining round covers a different nonce region.
	// Deterministic starts meant successive rounds all searched the same narrow region.
	startNonceBase := calculateStartNonceCPU(block, "random")
	
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
	if m.DebugLogging {
		log.Printf("DEBUG: Starting %d mining threads, %d hashes per thread, total=%d", m.ThreadCount, hashesPerThread, m.effectiveMaxHashes())
	}
	for t := 0; t < m.ThreadCount; t++ {
		wg.Add(1)
		go func(threadID int) {
			defer func() {
				if m.DebugLogging {
					log.Printf("DEBUG: Thread %d exiting", threadID)
				}
				wg.Done()
			}()

			// Pin goroutine to CPU core for better cache locality
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			if m.DebugLogging {
				log.Printf("DEBUG: Thread %d started", threadID)
			}
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

			// Log target and initial hash sample
			if m.DebugLogging && threadID == 0 {
				log.Printf("DEBUG CPU Mining: Target (LE) = %s", hex.EncodeToString(targetBytesLE))
			}

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
						hashLE := m.scryptHash(headerBuf) // scryptHash returns LE format directly - no reversal needed
						localHashCount++

						// Log sample hashes and target comparison every 1000 hashes
						if m.DebugLogging && localHashCount%1000 == 0 && threadID == 0 {
							log.Printf("DEBUG CPU: Nonce %d, Hash (LE) = %s, Meets target: %v",
								nonce, hex.EncodeToString(hashLE), hashMeetsTargetLE(hashLE, targetBytesLE))
						}

						if hashMeetsTargetLE(hashLE, targetBytesLE) {
							log.Printf("✅ CPU THREAD %d FOUND BLOCK: Nonce %d", threadID, nonce)
							if !found.Swap(true) {
								// Return hash in display order (reversed from LE) for logging
								hashDisplay := make([]byte, 32)
								copy(hashDisplay, hashLE)
								reverseBytesInPlace(hashDisplay)
								results <- struct {
									nonce uint32
									hash  []byte
								}{nonce, hashDisplay}
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
							hashLE := m.scryptHash(headerBuf) // scryptHash returns LE format directly - no reversal needed
							localHashCount++

							if hashMeetsTargetLE(hashLE, targetBytesLE) {
								if !found.Swap(true) {
									// Return hash in display order (reversed from LE) for logging
									hashDisplay := make([]byte, 32)
									copy(hashDisplay, hashLE)
									reverseBytesInPlace(hashDisplay)
									results <- struct {
										nonce uint32
										hash  []byte
									}{nonce, hashDisplay}
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
							hashLE := m.scryptHash(headerBuf) // scryptHash returns LE format directly - no reversal needed
							localHashCount++

							if hashMeetsTargetLE(hashLE, targetBytesLE) {
								if !found.Swap(true) {
									// Return hash in display order (reversed from LE) for logging
									hashDisplay := make([]byte, 32)
									copy(hashDisplay, hashLE)
									reverseBytesInPlace(hashDisplay)
									results <- struct {
										nonce uint32
										hash  []byte
									}{nonce, hashDisplay}
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

	// Wait for all threads to complete (no timeout - let them finish naturally)
	go func() {
		wg.Wait()
		close(results)
	}()

	// Try to get a result from any thread
	if m.DebugLogging {
		log.Printf("DEBUG: Entering loop to read from results channel")
	}
	for result := range results {
		if result.nonce != 0 && len(result.hash) > 0 {
			foundNonce = result.nonce
			foundHash = result.hash
			// Double-check the hash meets the target (fast comparison)
			targetBytes := targetBig.Bytes()
			targetBytesLE := make([]byte, 32)
			copy(targetBytesLE[32-len(targetBytes):], targetBytes)
			reverseBytesInPlace(targetBytesLE)
			if hashMeetsTargetLE(foundHash, targetBytesLE) {
				if m.DebugLogging {
					log.Printf("DEBUG: Found valid hash, draining remaining results...")
				}
				// Drain remaining results before returning
				go func() {
					for range results {
					}
				}()
				return foundNonce, foundHash, true, totalHashes.Load()
			}
		}
	}

	return 0, nil, false, totalHashes.Load()
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
//
// CRITICAL: scrypt.Key() returns 32 bytes in little-endian uint256 format directly.
// This is the NATIVE format that Dogecoin Core uses internally for PoW comparison.
// DO NOT reverse these bytes before comparison — they are already in the correct format.
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
