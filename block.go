package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

type Block struct {
	Version       int32
	PrevBlock     [32]byte
	MerkleRoot    [32]byte
	Timestamp     uint32
	Bits          uint32
	Nonce         uint32
	Transactions []*Transaction
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

	// Block header
	header := b.SerializeHeader()
	buf.Write(header)

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
