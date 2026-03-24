package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
)

type Block struct {
	Version       int32
	PrevBlock     [32]byte
	MerkleRoot    [32]byte
	Timestamp     uint32
	Bits          uint32
	Nonce         uint32
	AuxPow        *AuxPow           // Dogecoin merge-mining structure (required by protocol)
	Transactions []*Transaction
}

// AuxPow represents the complete merge-mining structure required by Dogecoin protocol
// Format: CMerkleTx (tx + block hash + merkle branch + index) + chain merkle branch + chain index + parent block header
type AuxPow struct {
	// CMerkleTx fields (transaction component)
	CoinbaseTx       *Transaction // The coinbase transaction from parent block
	BlockHash        [32]byte      // Hash of parent block (in wire order)
	MerkleBranch     [][]byte      // Merkle branch from tx to parent block root (empty for solo)
	MerkleIndex      uint32        // Index of tx in parent block merkle tree (0 for solo coinbase)

	// Chain merkle branch
	ChainMerkleBranch [][]byte // Merkle branch from aux block to chain (empty for solo)
	ChainIndex        uint32   // Index in merge-mining chain (0 for solo mining)

	// Parent block header (the actual PoW block) - reference to existing BlockHeader type from headers.go
	ParentBlockHeader *BlockHeader // Full header of parent block
}

type Transaction struct {
	Version  int32
	Inputs   []TxInput
	Outputs  []TxOutput
	LockTime uint32
}

type TxInput struct {
	PreviousOutput OutPoint
	ScriptSig      []byte
	Sequence       uint32
}

type TxOutput struct {
	Value        int64
	ScriptPubKey []byte
}

type OutPoint struct {
	Hash  [32]byte
	Index uint32
}

func (b *Block) SerializeHeader() []byte {
	var buf bytes.Buffer

	// Version (4 bytes, little-endian)
	binary.Write(&buf, binary.LittleEndian, b.Version)

	// Previous block hash (32 bytes)
	buf.Write(b.PrevBlock[:])

	// Merkle root (32 bytes)
	buf.Write(b.MerkleRoot[:])

	// Timestamp (4 bytes, little-endian)
	binary.Write(&buf, binary.LittleEndian, b.Timestamp)

	// Bits (4 bytes, little-endian)
	binary.Write(&buf, binary.LittleEndian, b.Bits)

	// Nonce (4 bytes, little-endian)
	binary.Write(&buf, binary.LittleEndian, b.Nonce)

	return buf.Bytes()
}

func (b *Block) Serialize() (string, error) {
	var buf bytes.Buffer

	// Block header (80 bytes)
	header := b.SerializeHeader()
	buf.Write(header)

	// DEBUG: Log exact header bytes and Bits value for peer comparison
	log.Printf("[SERIALIZE] Full header (80 bytes): %s", hex.EncodeToString(header))
	log.Printf("[SERIALIZE] Parsed from header - Version: 0x%08x, Bits: 0x%08x, Nonce: %d",
		binary.LittleEndian.Uint32(header[0:4]),
		binary.LittleEndian.Uint32(header[72:76]),
		binary.LittleEndian.Uint32(header[76:80]))

	// AuxPow data (required by Dogecoin protocol for merge-mining support)
	// For solo mining: serialize minimal AuxPow (just merkle branch count=0 and chain index=0)
	// For merge-mining: serialize full AuxPow structure
	if b.AuxPow != nil {
		b.AuxPow.Serialize(&buf)
	} else {
		// Minimal AuxPow for solo mining (no merge-mining)
		// This signals to peers that there is no AuxPow structure beyond these fields
		writeVarInt(&buf, 0)                              // 0 merkle branch hashes
		binary.Write(&buf, binary.LittleEndian, uint32(0)) // nChainIndex = 0
	}

	// Transaction count (varint)
	writeVarInt(&buf, uint64(len(b.Transactions)))

	// Transactions
	for _, tx := range b.Transactions {
		txData, err := tx.Serialize()
		if err != nil {
			return "", err
		}
		buf.Write(txData)
	}

	return hex.EncodeToString(buf.Bytes()), nil
}

func (tx *Transaction) Serialize() ([]byte, error) {
	var buf bytes.Buffer

	// Version (4 bytes, little-endian)
	binary.Write(&buf, binary.LittleEndian, tx.Version)

	// Input count (varint)
	writeVarInt(&buf, uint64(len(tx.Inputs)))

	// Inputs
	for _, input := range tx.Inputs {
		// Previous output hash (32 bytes)
		buf.Write(input.PreviousOutput.Hash[:])
		// Previous output index (4 bytes, little-endian)
		binary.Write(&buf, binary.LittleEndian, input.PreviousOutput.Index)
		// Script length (varint)
		writeVarInt(&buf, uint64(len(input.ScriptSig)))
		// Script
		buf.Write(input.ScriptSig)
		// Sequence (4 bytes, little-endian)
		binary.Write(&buf, binary.LittleEndian, input.Sequence)
	}

	// Output count (varint)
	writeVarInt(&buf, uint64(len(tx.Outputs)))

	// Outputs
	for _, output := range tx.Outputs {
		// Value (8 bytes, little-endian)
		binary.Write(&buf, binary.LittleEndian, output.Value)
		// Script length (varint)
		writeVarInt(&buf, uint64(len(output.ScriptPubKey)))
		// Script
		buf.Write(output.ScriptPubKey)
	}

	// Lock time (4 bytes, little-endian)
	binary.Write(&buf, binary.LittleEndian, tx.LockTime)

	return buf.Bytes(), nil
}

func (tx *Transaction) GetHash() ([32]byte, error) {
	data, err := tx.Serialize()
	if err != nil {
		return [32]byte{}, fmt.Errorf("failed to serialize transaction: %v", err)
	}
	hash := sha256.Sum256(data)
	hash2 := sha256.Sum256(hash[:])
	return hash2, nil
}

// Serialize encodes AuxPow data for network transmission
// Format: CMerkleTx (tx + blockHash + merkleBranch + merkleIndex) + chainMerkleBranch + chainIndex + parentBlockHeader
// Follows Dogecoin Core CAuxPow serialization exactly
func (ap *AuxPow) Serialize(buf *bytes.Buffer) {
	// === CMerkleTx Component ===

	// 1. Serialize the coinbase transaction from parent block (required, even for solo mining)
	// For solo mining, serialize an empty transaction (version=0, no inputs/outputs, locktime=0)
	if ap.CoinbaseTx != nil {
		txData, _ := ap.CoinbaseTx.Serialize()
		buf.Write(txData)
	} else {
		// Empty transaction for solo mining: version (4) + 0 inputs (1 byte) + 0 outputs (1 byte) + locktime (4)
		binary.Write(buf, binary.LittleEndian, int32(0)) // Version
		writeVarInt(buf, 0)                               // Input count = 0
		writeVarInt(buf, 0)                               // Output count = 0
		binary.Write(buf, binary.LittleEndian, uint32(0)) // LockTime
	}

	// 2. Serialize parent block hash (32 bytes, in wire order)
	buf.Write(ap.BlockHash[:])

	// 3. Serialize transaction merkle branch (count + hashes)
	// This is the merkle branch from the tx to the parent block's merkle root
	// Empty for solo mining
	writeVarInt(buf, uint64(len(ap.MerkleBranch)))
	for _, hash := range ap.MerkleBranch {
		buf.Write(hash)
	}

	// 4. Serialize merkle tree index (position of tx in parent block)
	// 0 for solo mining (coinbase is always at index 0)
	binary.Write(buf, binary.LittleEndian, ap.MerkleIndex)

	// === Chain Merkle Branch Component ===

	// 5. Serialize chain merkle branch (count + hashes)
	// Empty for solo mining, non-empty for merge-mining
	writeVarInt(buf, uint64(len(ap.ChainMerkleBranch)))
	for _, hash := range ap.ChainMerkleBranch {
		buf.Write(hash)
	}

	// 6. Serialize chain index (0 for solo mining)
	binary.Write(buf, binary.LittleEndian, ap.ChainIndex)

	// === Parent Block Header Component ===

	// 7. Serialize parent block header (80 bytes)
	// For solo mining, serialize a zero header (all bytes = 0)
	if ap.ParentBlockHeader != nil {
		buf.Write(ap.ParentBlockHeader.Serialize())
	} else {
		// Zero header for solo mining (80 zero bytes)
		buf.Write(make([]byte, 80))
	}
}

func writeVarInt(buf *bytes.Buffer, val uint64) {
	if val < 0xFD {
		buf.WriteByte(byte(val))
	} else if val <= 0xFFFF {
		buf.WriteByte(0xFD)
		binary.Write(buf, binary.LittleEndian, uint16(val))
	} else if val <= 0xFFFFFFFF {
		buf.WriteByte(0xFE)
		binary.Write(buf, binary.LittleEndian, uint32(val))
	} else {
		buf.WriteByte(0xFF)
		binary.Write(buf, binary.LittleEndian, val)
	}
}

// Helper function to convert hex string to 32-byte array
func hexTo32Bytes(s string) ([32]byte, error) {
	var result [32]byte
	decoded, err := hex.DecodeString(s)
	if err != nil {
		return result, err
	}
	if len(decoded) != 32 {
		return result, fmt.Errorf("invalid length: expected 32 bytes, got %d", len(decoded))
	}
	copy(result[:], decoded)
	// Reverse bytes (Dogecoin uses little-endian for block hashes)
	for i := 0; i < 16; i++ {
		result[i], result[31-i] = result[31-i], result[i]
	}
	return result, nil
}
