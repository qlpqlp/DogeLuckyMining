package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"
)

type HeaderFetcher struct {
	rpcClient   *RPCClient
	headers     map[int64]*BlockHeader
	headersMutex sync.RWMutex
	lastHeight  int64
	stopChan    chan struct{}
	wg          sync.WaitGroup
}

type BlockHeader struct {
	Version    int32
	PrevBlock  [32]byte
	MerkleRoot [32]byte
	Timestamp  uint32
	Bits       uint32
	Nonce      uint32
	Height     int64
	Hash       [32]byte
}

func NewHeaderFetcher(rpcClient *RPCClient) *HeaderFetcher {
	return &HeaderFetcher{
		rpcClient:  rpcClient,
		headers:    make(map[int64]*BlockHeader),
		lastHeight: -1,
		stopChan:   make(chan struct{}),
	}
}

func (hf *HeaderFetcher) Start() {
	hf.wg.Add(1)
	go hf.fetchLoop()
}

func (hf *HeaderFetcher) Stop() {
	close(hf.stopChan)
	hf.wg.Wait()
}

func (hf *HeaderFetcher) fetchLoop() {
	defer hf.wg.Done()

	ticker := time.NewTicker(30 * time.Second) // Less frequent updates
	defer ticker.Stop()

	// Initial fetch with delay
	time.Sleep(2 * time.Second)
	hf.fetchHeaders()

	for {
		select {
		case <-hf.stopChan:
			return
		case <-ticker.C:
			hf.fetchHeaders()
		}
	}
}

func (hf *HeaderFetcher) fetchHeaders() {
	// Get current block count
	blockCount, err := hf.rpcClient.GetBlockCount()
	if err != nil {
		log.Printf("Error getting block count: %v", err)
		return
	}

	// Fetch new headers if height increased
	if blockCount > hf.lastHeight {
		// Fetch headers from last known height to current
		startHeight := hf.lastHeight + 1
		if hf.lastHeight < 0 {
			// For mining, we only need the current block - start from the most recent block
			// Don't fetch historical blocks, just track new ones as they come
			startHeight = blockCount // Start from current block only
			log.Printf("Starting header tracking from current block height %d (no historical fetch)", startHeight)
		}
		
		// Limit how many headers we fetch in one go to avoid overwhelming the RPC
		maxHeadersPerFetch := 20
		endHeight := blockCount
		if endHeight - startHeight > int64(maxHeadersPerFetch) {
			endHeight = startHeight + int64(maxHeadersPerFetch)
			log.Printf("Limiting header fetch to %d headers (from %d to %d)", maxHeadersPerFetch, startHeight, endHeight)
		}

		successCount := 0
		errorCount := 0
		for height := startHeight; height <= endHeight; height++ {
			header, err := hf.fetchHeader(height)
			if err != nil {
				errorCount++
				// Log first few errors, then reduce logging
				if errorCount <= 3 {
					log.Printf("Error fetching header at height %d: %v", height, err)
				}
				// If too many consecutive errors, stop trying
				if errorCount > 10 {
					log.Printf("Too many header fetch errors (%d), stopping at height %d", errorCount, height)
					break
				}
				// Small delay between failed attempts
				time.Sleep(100 * time.Millisecond)
				continue
			}

			hf.headersMutex.Lock()
			hf.headers[height] = header
			hf.headersMutex.Unlock()
			successCount++
			errorCount = 0 // Reset error count on success
		}

		// Update last height to what we actually fetched
		if successCount > 0 {
			hf.lastHeight = startHeight + int64(successCount) - 1
			log.Printf("Fetched %d headers up to height %d", successCount, hf.lastHeight)
		} else if endHeight < blockCount {
			// If we limited the fetch, update to the end of what we tried
			hf.lastHeight = endHeight
		}
	}
}

func (hf *HeaderFetcher) fetchHeader(height int64) (*BlockHeader, error) {
	hash, err := hf.rpcClient.GetBlockHash(height)
	if err != nil {
		return nil, err
	}
	return ParseBlockHeaderFromRPC(hf.rpcClient, hash, height)
}

// ParseBlockHeaderFromRPC parses getblockheader RPC result into a BlockHeader.
// Used by HeaderFetcher and by RPC GetTipHeader.
func ParseBlockHeaderFromRPC(c *RPCClient, blockHash string, height int64) (*BlockHeader, error) {
	headerResp, err := c.Call("getblockheader", []interface{}{blockHash, true})
	if err != nil {
		headerResp, err = c.Call("getblockheader", []interface{}{blockHash})
		if err != nil {
			return nil, fmt.Errorf("failed to get block header: %v", err)
		}
	}

	// Parse header from response - result can be a map or a string
	var headerMap map[string]interface{}
	
	// Try to parse as map first (verbose mode)
	if resultMap, ok := headerResp.Result.(map[string]interface{}); ok {
		headerMap = resultMap
	} else if resultStr, ok := headerResp.Result.(string); ok {
		// If it's a string, it's hex-encoded header, decode it
		headerBytes, err := hex.DecodeString(resultStr)
		if err != nil {
			return nil, fmt.Errorf("failed to decode header hex: %v", err)
		}
		// Parse binary header (80 bytes)
		if len(headerBytes) < 80 {
			return nil, fmt.Errorf("header too short: %d bytes", len(headerBytes))
		}
		
		header := &BlockHeader{
			Height: height,
		}
		
		// Parse binary header format
		header.Version = int32(binary.LittleEndian.Uint32(headerBytes[0:4]))
		// Copy and reverse bytes for PrevBlock and MerkleRoot
		prevBlockBytes := make([]byte, 32)
		copy(prevBlockBytes, headerBytes[4:36])
		copy(header.PrevBlock[:], reverseBytes(prevBlockBytes))
		
		merkleRootBytes := make([]byte, 32)
		copy(merkleRootBytes, headerBytes[36:68])
		copy(header.MerkleRoot[:], reverseBytes(merkleRootBytes))
		header.Timestamp = binary.LittleEndian.Uint32(headerBytes[68:72])
		header.Bits = binary.LittleEndian.Uint32(headerBytes[72:76])
		header.Nonce = binary.LittleEndian.Uint32(headerBytes[76:80])
		
	// Calculate hash
	hashBytes, err := hex.DecodeString(blockHash)
	if err == nil && len(hashBytes) == 32 {
		// Reverse bytes (little-endian to big-endian for storage)
		reversed := make([]byte, 32)
		for i := 0; i < 32; i++ {
			reversed[i] = hashBytes[31-i]
		}
		copy(header.Hash[:], reversed)
		}
		
		return header, nil
	} else {
		// Log the actual type for debugging
		log.Printf("Warning: Unexpected header response type at height %d: %T, value: %v", height, headerResp.Result, headerResp.Result)
		return nil, fmt.Errorf("invalid header response type: %T (expected map or string)", headerResp.Result)
	}

	header := &BlockHeader{
		Height: height,
	}

	// Parse version
	if v, ok := headerMap["version"].(float64); ok {
		header.Version = int32(v)
	}

	// Parse previous block hash
	if prevHash, ok := headerMap["previousblockhash"].(string); ok {
		prevBlock, err := hexTo32Bytes(prevHash)
		if err == nil {
			header.PrevBlock = prevBlock
		}
	}

	// Parse merkle root
	if merkleRoot, ok := headerMap["merkleroot"].(string); ok {
		root, err := hexTo32Bytes(merkleRoot)
		if err == nil {
			header.MerkleRoot = root
		}
	}

	// Parse timestamp
	if timeVal, ok := headerMap["time"].(float64); ok {
		header.Timestamp = uint32(timeVal)
	}

	// Parse bits - can be string or number
	if bitsStr, ok := headerMap["bits"].(string); ok {
		// Bits in RPC are a compact uint32 in hex (big-endian textual form).
		// Parse directly instead of assuming 8-char / little-endian layout.
		if v, err := strconv.ParseUint(bitsStr, 16, 32); err == nil {
			header.Bits = uint32(v)
		} else {
			log.Printf("Warning: failed to parse bits '%s' at height %d: %v", bitsStr, height, err)
		}
	} else if bitsNum, ok := headerMap["bits"].(float64); ok {
		header.Bits = uint32(bitsNum)
	}

	// Parse nonce
	if nonce, ok := headerMap["nonce"].(float64); ok {
		header.Nonce = uint32(nonce)
	}

	// Calculate hash
	hashBytes, err := hex.DecodeString(blockHash)
	if err == nil && len(hashBytes) == 32 {
		// Reverse bytes (little-endian to big-endian for storage)
		reversed := make([]byte, 32)
		for i := 0; i < 32; i++ {
			reversed[i] = hashBytes[31-i]
		}
		copy(header.Hash[:], reversed)
	}

	return header, nil
}

func (hf *HeaderFetcher) GetHeader(height int64) *BlockHeader {
	hf.headersMutex.RLock()
	defer hf.headersMutex.RUnlock()
	return hf.headers[height]
}

func (hf *HeaderFetcher) GetLastHeight() int64 {
	hf.headersMutex.RLock()
	defer hf.headersMutex.RUnlock()
	return hf.lastHeight
}

func (hf *HeaderFetcher) GetHeadersCount() int {
	hf.headersMutex.RLock()
	defer hf.headersMutex.RUnlock()
	return len(hf.headers)
}

// Serialize returns the 80-byte binary serialization of the block header
func (bh *BlockHeader) Serialize() []byte {
	var buf [80]byte
	binary.LittleEndian.PutUint32(buf[0:4], uint32(bh.Version))
	copy(buf[4:36], bh.PrevBlock[:])
	copy(buf[36:68], bh.MerkleRoot[:])
	binary.LittleEndian.PutUint32(buf[68:72], bh.Timestamp)
	binary.LittleEndian.PutUint32(buf[72:76], bh.Bits)
	binary.LittleEndian.PutUint32(buf[76:80], bh.Nonce)
	return buf[:]
}
