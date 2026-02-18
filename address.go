package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
)

const (
	base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	// Dogecoin address prefixes
	mainnetP2PKHPrefix = byte(0x1e) // 'D' addresses
	testnetP2PKHPrefix = byte(0x71) // 'n'/'m' addresses
)

// base58Decode decodes a Base58 string to bytes
func base58Decode(s string) ([]byte, error) {
	// Convert string to big integer
	bigInt := big.NewInt(0)
	for _, char := range s {
		idx := -1
		for i, c := range base58Alphabet {
			if c == char {
				idx = i
				break
			}
		}
		if idx == -1 {
			return nil, fmt.Errorf("invalid base58 character: %c", char)
		}
		bigInt.Mul(bigInt, big.NewInt(58))
		bigInt.Add(bigInt, big.NewInt(int64(idx)))
	}

	// Convert big integer to bytes
	bytes := bigInt.Bytes()
	
	// Handle leading zeros (Base58 preserves them)
	leadingZeros := 0
	for i := 0; i < len(s) && s[i] == '1'; i++ {
		leadingZeros++
	}
	
	result := make([]byte, leadingZeros+len(bytes))
	copy(result[leadingZeros:], bytes)
	
	return result, nil
}

// base58DecodeCheck decodes a Base58Check string (with checksum verification)
func base58DecodeCheck(s string) ([]byte, error) {
	decoded, err := base58Decode(s)
	if err != nil {
		return nil, err
	}
	
	if len(decoded) < 5 {
		return nil, fmt.Errorf("address too short")
	}
	
	// Split payload and checksum
	payload := decoded[:len(decoded)-4]
	checksum := decoded[len(decoded)-4:]
	
	// Verify checksum
	hash := sha256.Sum256(payload)
	hash = sha256.Sum256(hash[:])
	
	if hash[0] != checksum[0] || hash[1] != checksum[1] || 
	   hash[2] != checksum[2] || hash[3] != checksum[3] {
		return nil, fmt.Errorf("invalid checksum")
	}
	
	return payload, nil
}

// DecodeDogecoinAddress decodes a Dogecoin address to scriptPubKey
func DecodeDogecoinAddress(address string) ([]byte, error) {
	if len(address) < 26 || len(address) > 35 {
		return nil, fmt.Errorf("invalid address length")
	}
	
	// Decode Base58Check
	decoded, err := base58DecodeCheck(address)
	if err != nil {
		return nil, fmt.Errorf("failed to decode address: %v", err)
	}
	
	if len(decoded) < 21 {
		return nil, fmt.Errorf("decoded address too short")
	}
	
	// Extract version byte and hash160
	version := decoded[0]
	hash160 := decoded[1:21]
	
	// Verify version byte (mainnet or testnet P2PKH)
	if version != mainnetP2PKHPrefix && version != testnetP2PKHPrefix {
		return nil, fmt.Errorf("unsupported address type (version: 0x%02x)", version)
	}
	
	// Create P2PKH script from hash160
	script := CreateP2PKHScript(hash160)
	if script == nil {
		return nil, fmt.Errorf("failed to create P2PKH script")
	}
	
	return script, nil
}

// CreateP2PKHScript creates a Pay-to-PubKey-Hash script from a 20-byte hash
func CreateP2PKHScript(hash160 []byte) []byte {
	if len(hash160) != 20 {
		return nil
	}
	
	// OP_DUP OP_HASH160 <20-byte hash> OP_EQUALVERIFY OP_CHECKSIG
	script := make([]byte, 0, 25)
	script = append(script, 0x76) // OP_DUP
	script = append(script, 0xA9) // OP_HASH160
	script = append(script, 0x14) // Push 20 bytes
	script = append(script, hash160...)
	script = append(script, 0x88) // OP_EQUALVERIFY
	script = append(script, 0xAC) // OP_CHECKSIG
	
	return script
}

// HexToHash160 converts a hex string to a 20-byte hash
func HexToHash160(hexStr string) ([]byte, error) {
	bytes, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, err
	}
	if len(bytes) != 20 {
		return nil, fmt.Errorf("invalid hash160 length: expected 20 bytes, got %d", len(bytes))
	}
	return bytes, nil
}
