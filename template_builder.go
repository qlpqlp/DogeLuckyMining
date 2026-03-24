package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"
)

// Block reward in base units (1 DOGE = 100,000,000 base units).
// From block 600,001 onward: fixed 10,000 DOGE per block.
const (
	dogecoinBlockRewardFrom600k = 1000000000000 // 10,000 DOGE in base units
	dogecoinRewardChangeHeight  = 600001
)

// BlockRewardForHeight returns the coinbase value in base units for the given block height.
// Dogecoin: fixed 10,000 DOGE from block 600,001+; earlier blocks had variable rewards.
func BlockRewardForHeight(height int64) int64 {
	if height >= dogecoinRewardChangeHeight {
		return dogecoinBlockRewardFrom600k
	}
	// Historic schedule (simplified): use 10k for simplicity; real schedule is random 0–N per era
	// For mining current chain we only care about height >= 600001
	return dogecoinBlockRewardFrom600k
}

// BitsToTarget converts compact "bits" (uint32) to a 32-byte big-endian target.
// Bitcoin/Dogecoin compact format: size (1 byte) + mantissa (3 bytes), target = mantissa * 256^(size-3).
func BitsToTarget(bits uint32) *big.Int {
	size := bits >> 24
	word := bits & 0x00FFFFFF
	if size <= 3 {
		// target = word >> (8*(3-size))
		shift := 8 * (3 - size)
		return new(big.Int).Rsh(big.NewInt(int64(word)), uint(shift))
	}
	// target = word << (8*(size-3))
	shift := 8 * (size - 3)
	return new(big.Int).Lsh(big.NewInt(int64(word)), uint(shift))
}

// BitsToTargetHex returns the target as a hex string (32 bytes big-endian), as used by the miner.
func BitsToTargetHex(bits uint32) string {
	t := BitsToTarget(bits)
	b := t.Bytes()
	if len(b) > 32 {
		b = b[len(b)-32:]
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return hex.EncodeToString(out)
}

// Difficulty 1 target (max target) for Bitcoin/Dogecoin: 0x00000000ffff0000...
var difficulty1Target = new(big.Int).SetBytes([]byte{
	0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
})

// BitsToDifficulty returns the chain difficulty from the compact "bits" field.
// difficulty = difficulty_1_target / current_target (so it works for both RPC and P2P).
func BitsToDifficulty(bits uint32) float64 {
	current := BitsToTarget(bits)
	if current.Sign() <= 0 {
		return 0
	}
	// difficulty_1_target / current_target as float64
	q := new(big.Float).SetInt(difficulty1Target)
	q.Quo(q, new(big.Float).SetInt(current))
	d, _ := q.Float64()
	return d
}

// BuildTemplateFromHeader builds a BlockTemplate from the chain tip header.
// This allows the app to build the block template in-app using only the tip (from RPC or P2P).
// network is "mainnet" or "testnet"; used to compute correct next-block bits (e.g. testnet min-difficulty).
// lastNonMinBits: on testnet P2P, pass the last seen non-min-difficulty nBits so we use correct bits when tip is min-diff (can be nil for RPC).
// tipParentTimestamp: parent block's unix time (P2P); 0 if unknown (DigiShield uses tip.Bits fallback).
func BuildTemplateFromHeader(tip *BlockHeader, payoutAddr string, network string, lastNonMinBits *uint32, tipParentTimestamp uint32) (*BlockTemplate, error) {
	if tip == nil {
		return nil, fmt.Errorf("tip header is nil")
	}
	nextHeight := tip.Height + 1
	now := time.Now().Unix()

	// Required bits for the *next* block (DigiShield on mainnet/testnet when tip ≥ 145000; testnet min-diff rules)
	nextBits := GetNextWorkRequiredExtended(tip, now, network, lastNonMinBits, tipParentTimestamp)

	// Previous block hash in display/BE order (same as RPC getblocktemplate); createEmptyBlock hexTo32Bytes reverses to internal for wire
	prevDisplay := make([]byte, 32)
	copy(prevDisplay, tip.Hash[:])
	reverseBytesInPlace(prevDisplay)
	prevHashHex := hex.EncodeToString(prevDisplay)

	// Bits: 4 bytes little-endian as 8-char hex
	bitsBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(bitsBuf, nextBits)
	bitsHex := hex.EncodeToString(bitsBuf)

	targetHex := BitsToTargetHex(nextBits)

	// For solo mining: SET the AuxPoW bit (0x100)
	// The 0x100 bit tells peers: "AuxPow data follows the 80-byte header"
	// We serialize minimal AuxPow (5 bytes: merkle count=0, chain index=0)
	// Peers read AuxPow even for solo blocks (they just see zeros)
	// This bit MUST be set whenever ANY AuxPow data is serialized
	nextVersion := tip.Version | int32(0x100) // Force set VERSION_AUXPOW bit

	return &BlockTemplate{
		Version:           nextVersion,
		PreviousBlockHash: prevHashHex,
		Transactions:      nil,
		CoinbaseValue:     BlockRewardForHeight(nextHeight),
		CoinbaseScript:    []byte{0x00},
		CoinbaseAux:       nil,
		Target:            targetHex,
		Bits:              bitsHex,
		Height:            nextHeight,
		Difficulty:        BitsToDifficulty(nextBits),
		Mintime:           int64(tip.Timestamp),
		CurTime:           now,
		NonceRange:        "",
	}, nil
}

// FormatTemplateBitsForLog decodes template.Bits (8 hex chars = 4 LE bytes on wire) to canonical compact nBits for logs.
// Example: wire hex "ffff0f1e" is the same nBits as Core prints as 0x1e0fffff.
func FormatTemplateBitsForLog(bitsHex string) string {
	b, err := hex.DecodeString(bitsHex)
	if err != nil || len(b) != 4 {
		return bitsHex
	}
	compact := binary.LittleEndian.Uint32(b)
	return fmt.Sprintf("nBits(compact)=0x%08x wire-bytes-hex=%s", compact, bitsHex)
}
