package main

import (
	"encoding/binary"
	"encoding/hex"
	"log"
)

// HashToDisplayHex returns the block hash in display order (as shown in block explorers).
// Our internal/wire order is the reverse of display order.
func HashToDisplayHex(h []byte) string {
	if len(h) != 32 {
		return hex.EncodeToString(h)
	}
	b := make([]byte, 32)
	copy(b, h)
	reverseBytesInPlace(b)
	return hex.EncodeToString(b)
}

// hashMeetsTargetFast performs fast byte-by-byte comparison without big.Int allocation.
// Both hash and target are 32-byte arrays in big-endian format.
// Returns true if hash < target (hash meets target).
func hashMeetsTargetFast(hash []byte, target []byte) bool {
	if len(hash) != 32 || len(target) != 32 {
		return false
	}
	for i := 0; i < 32; i++ {
		if hash[i] < target[i] {
			return true
		}
		if hash[i] > target[i] {
			return false
		}
	}
	return false
}

// hashMeetsTargetLE compares two 256-bit values in little-endian byte order (index 0 = LSB).
// Used for Dogecoin/Litecoin PoW where Core uses uint256 LE. Returns true if hashLE <= targetLE (valid block).
func hashMeetsTargetLE(hashLE []byte, targetLE []byte) bool {
	if len(hashLE) != 32 || len(targetLE) != 32 {
		return false
	}
	// Compare from most significant byte (index 31) to least (index 0)
	for i := 31; i >= 0; i-- {
		if hashLE[i] < targetLE[i] {
			return true
		}
		if hashLE[i] > targetLE[i] {
			return false
		}
	}
	// All bytes equal: hash == target — valid (Core: !(hash > target), i.e. hash <= target)
	return true
}

// reverseBytesInPlace reverses bytes in place (no allocation)
// WARNING: Modifies the input slice!
func reverseBytesInPlace(b []byte) {
	for i := 0; i < len(b)/2; i++ {
		j := len(b) - 1 - i
		b[i], b[j] = b[j], b[i]
	}
}

// tipHashDisplayHex is the block hash of the tip header in explorer/display order (matches template PreviousBlockHash).
func tipHashDisplayHex(tip *BlockHeader) string {
	if tip == nil {
		return ""
	}
	return HashToDisplayHex(tip.Hash[:])
}

// logHeaderDebug logs the full 80-byte block header in detailed format for debugging
func logHeaderDebug(label string, header []byte) {
	if len(header) != 80 {
		return // Silently skip if not 80 bytes
	}
	// Parse components
	version := binary.LittleEndian.Uint32(header[0:4])
	prevBlock := hex.EncodeToString(header[4:36])
	merkleRoot := hex.EncodeToString(header[36:68])
	timestamp := binary.LittleEndian.Uint32(header[68:72])
	bits := binary.LittleEndian.Uint32(header[72:76])
	nonce := binary.LittleEndian.Uint32(header[76:80])

	log.Printf("DEBUG %s (80 bytes):", label)
	log.Printf("  Hex: %s", hex.EncodeToString(header))
	log.Printf("  Version: 0x%08x, Bits: 0x%08x, Nonce: %d", version, bits, nonce)
	log.Printf("  PrevBlock: %s", prevBlock[:32]+"...")
	log.Printf("  MerkleRoot: %s", merkleRoot[:32]+"...")
	log.Printf("  Timestamp: %d", timestamp)
}
