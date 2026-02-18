package main

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// buildWireHeader builds 80-byte block header in wire order (LE version/time/bits/nonce, hashes in internal order).
// prevHex and merkleHex are 32-byte hashes in display (big-endian) hex; we reverse for wire.
func buildWireHeader(version int32, prevHex, merkleHex string, timestamp, bits, nonce uint32) []byte {
	prev, _ := hex.DecodeString(prevHex)
	merkle, _ := hex.DecodeString(merkleHex)
	if len(prev) != 32 || len(merkle) != 32 {
		panic("prev/merkle must be 32 bytes hex")
	}
	reverseBytesInPlace(prev)
	reverseBytesInPlace(merkle)
	out := make([]byte, 80)
	binary.LittleEndian.PutUint32(out[0:4], uint32(version))
	copy(out[4:36], prev)
	copy(out[36:68], merkle)
	binary.LittleEndian.PutUint32(out[68:72], timestamp)
	binary.LittleEndian.PutUint32(out[72:76], bits)
	binary.LittleEndian.PutUint32(out[76:80], nonce)
	return out
}

// TestScryptKnownBlockWire verifies our scrypt(80,80) on the wire-format header for Dogecoin block #100000.
// Wire order: version LE, prevBlock (internal/LE), merkle (internal/LE), time LE, bits LE, nonce LE.
// Expected scrypt output from Dogecoin Core / cpuminer on the same 80-byte header.
func TestScryptKnownBlockWire(t *testing.T) {
	// Block 100000: https://dogechain.info/block/100000
	// Version 2, prev 12aca093..., merkle 31757c26..., time 0x52fd869d, bits 0x1b267eeb, nonce 0x84214800
	header80 := buildWireHeader(
		2,
		"12aca0938fe1fb786c9e0e4375900e8333123de75e240abd3337d1b411d14ebe",
		"31757c266102d1bee62ef2ff8438663107d64bdd5d9d9173421ec25fb2a814de",
		0x52fd869d,
		0x1b267eeb,
		0x84214800,
	)
	if len(header80) != 80 {
		t.Fatalf("header length %d, want 80", len(header80))
	}

	m := NewScryptMiner()
	got := m.scryptHash(header80)
	if len(got) != 32 {
		t.Fatalf("scrypt output length %d, want 32", len(got))
	}

	// Scrypt output is in internal/LE order; block explorers show display order (reversed).
	// Cartesi doc expected hash is in display order: 00000000002647462b1abb10059b1f6f363acbc93f581cc256cc208e0895e5c7
	expectedDisplayHex := "00000000002647462b1abb10059b1f6f363acbc93f581cc256cc208e0895e5c7"
	gotReversed := make([]byte, 32)
	copy(gotReversed, got)
	reverseBytesInPlace(gotReversed)
	gotDisplayHex := hex.EncodeToString(gotReversed)
	if gotDisplayHex != expectedDisplayHex {
		t.Errorf("scrypt(wire block100000) display hash = %s, want %s", gotDisplayHex, expectedDisplayHex)
	}
}

// TestScryptKnownBlockFromBlock verifies the full path: Block -> SerializeHeader() -> scrypt -> known hash.
// Uses Dogecoin block #100000; asserts that our Block type and SerializeHeader() produce the same
// 80-byte wire format as buildWireHeader, and that scrypt of that header matches the reference.
func TestScryptKnownBlockFromBlock(t *testing.T) {
	// Block 100000: same values as TestScryptKnownBlockWire
	prevDisplayHex := "12aca0938fe1fb786c9e0e4375900e8333123de75e240abd3337d1b411d14ebe"
	merkleDisplayHex := "31757c266102d1bee62ef2ff8438663107d64bdd5d9d9173421ec25fb2a814de"
	prevInternal := decodeReverseHex(t, prevDisplayHex)
	merkleInternal := decodeReverseHex(t, merkleDisplayHex)

	block := &Block{
		Version:    2,
		PrevBlock:  must32(prevInternal),
		MerkleRoot: must32(merkleInternal),
		Timestamp:  0x52fd869d,
		Bits:       0x1b267eeb,
		Nonce:      0x84214800,
	}

	header80 := block.SerializeHeader()
	if len(header80) != 80 {
		t.Fatalf("SerializeHeader length %d, want 80", len(header80))
	}

	// SerializeHeader() must match wire layout (same bytes as buildWireHeader)
	expectedWire := buildWireHeader(2, prevDisplayHex, merkleDisplayHex, 0x52fd869d, 0x1b267eeb, 0x84214800)
	if hex.EncodeToString(header80) != hex.EncodeToString(expectedWire) {
		t.Errorf("Block.SerializeHeader() != buildWireHeader:\n  got    %s\n  want   %s",
			hex.EncodeToString(header80), hex.EncodeToString(expectedWire))
	}

	m := NewScryptMiner()
	got := m.scryptHash(header80)
	gotReversed := make([]byte, 32)
	copy(gotReversed, got)
	reverseBytesInPlace(gotReversed)
	gotDisplayHex := hex.EncodeToString(gotReversed)
	expectedDisplayHex := "00000000002647462b1abb10059b1f6f363acbc93f581cc256cc208e0895e5c7"
	if gotDisplayHex != expectedDisplayHex {
		t.Errorf("scrypt(Block.SerializeHeader()) display hash = %s, want %s", gotDisplayHex, expectedDisplayHex)
	}
}

// decodeReverseHex decodes 32-byte hex and returns it in internal/wire order (reversed).
func decodeReverseHex(t *testing.T, h string) []byte {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("decode hex %q: %v", h, err)
	}
	if len(b) != 32 {
		t.Fatalf("hex %q decoded to %d bytes, want 32", h, len(b))
	}
	reverseBytesInPlace(b)
	return b
}

func must32(b []byte) [32]byte {
	var a [32]byte
	copy(a[:], b)
	return a
}

// TestScryptKnownBlockCartesi is a duplicate of TestScryptKnownBlockWire: Cartesi doc expected hash
// is the same value in display order. Our wire-order header produces that hash (in LE we get the
// reversed bytes). Skipped so we only maintain one vector.
func TestScryptKnownBlockCartesi(t *testing.T) {
	t.Skip("Cartesi expected hash matches wire-order test; see TestScryptKnownBlockWire")
}
