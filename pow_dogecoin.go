package main

import (
	"encoding/hex"
	"log"
	"math/big"
)

// Dogecoin testnet min-difficulty (powLimit) compact bits. Same as genesis 0x1e0ffff0.
const dogecoinPowLimitCompact uint32 = 0x1e0ffff0

// Testnet allows min-difficulty block when block time > tip + 2*nPowTargetSpacing (2*60 = 120 sec).
const testnetMinDifficultySpacing = 120

// Dogecoin mainnet/testnet pow limit (Consensus::Params::powLimit) as big-endian 256-bit integer.
// Same as Core: uint256S("0x00000fffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
var dogecoinPowLimitUint256 *big.Int

func init() {
	const powHex = "00000fffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	b, err := hex.DecodeString(powHex)
	if err != nil || len(b) != 32 {
		panic("dogecoin pow limit hex")
	}
	dogecoinPowLimitUint256 = new(big.Int).SetBytes(b)
}

// dogecoinDigishieldStartHeight is the first block height mined with DigiShield rules (tip height >= this).
const dogecoinDigishieldStartHeight = 145000

// BigToCompact encodes a positive target as Bitcoin/Dogecoin nBits (compact unsigned form).
func BigToCompact(n *big.Int) uint32 {
	if n == nil || n.Sign() <= 0 {
		return 0
	}
	mantissa := new(big.Int).Set(n)
	exponent := len(mantissa.Bytes())
	if exponent <= 3 {
		compact := uint32(mantissa.Uint64() << (8 * (3 - exponent)))
		return compact
	}
	mantissa = new(big.Int).Rsh(n, uint(8*(exponent-3)))
	compact := uint32(mantissa.Uint64())
	if compact&0x00800000 != 0 {
		compact >>= 8
		exponent++
	}
	return uint32(exponent)<<24 | compact&0x007fffff
}

// calculateDogecoinDigishieldNextWork implements CalculateDogecoinNextWorkRequired when fDigishieldDifficultyCalculation is true.
// tipBits: nBits of chain tip (pindexLast). tipTime, parentTime: block times of tip and its parent (seconds).
func calculateDogecoinDigishieldNextWork(tipBits uint32, tipTime, parentTime int64) uint32 {
	const retargetTimespan int64 = 60
	nActualTimespan := tipTime - parentTime
	if nActualTimespan < 1 {
		nActualTimespan = 1
	}
	nModulatedTimespan := retargetTimespan + (nActualTimespan-retargetTimespan)/8
	nMinTimespan := retargetTimespan - (retargetTimespan / 4)
	nMaxTimespan := retargetTimespan + (retargetTimespan / 2)
	if nModulatedTimespan < nMinTimespan {
		nModulatedTimespan = nMinTimespan
	} else if nModulatedTimespan > nMaxTimespan {
		nModulatedTimespan = nMaxTimespan
	}
	bnOld := BitsToTarget(tipBits)
	if bnOld.Sign() <= 0 {
		return tipBits
	}
	bnNew := new(big.Int).Mul(bnOld, big.NewInt(nModulatedTimespan))
	bnNew.Div(bnNew, big.NewInt(retargetTimespan))
	if bnNew.Cmp(dogecoinPowLimitUint256) > 0 {
		bnNew = new(big.Int).Set(dogecoinPowLimitUint256)
	}
	return BigToCompact(bnNew)
}

// GetNextWorkRequiredExtended matches Dogecoin Core GetNextWorkRequired for current tip → next block bits.
// tipParentTimestamp: unix seconds on wire for the block parent of tip; 0 means unknown (falls back to tip.Bits for DigiShield).
// lastNonMinBits: testnet ≥157500 when tip is min-difficulty and block time does not allow another min-diff block.
func GetNextWorkRequiredExtended(tip *BlockHeader, blockTime int64, network string, lastNonMinBits *uint32, tipParentTimestamp uint32) uint32 {
	if tip == nil {
		return dogecoinPowLimitCompact
	}

	// Testnet: min-difficulty blocks (height ≥ 157500)
	if network == "testnet" && tip.Height >= 157500 {
		if blockTime > int64(tip.Timestamp)+testnetMinDifficultySpacing {
			return dogecoinPowLimitCompact
		}
		if tip.Bits == dogecoinPowLimitCompact {
			if lastNonMinBits != nil && *lastNonMinBits != 0 {
				log.Printf("P2P template: using lastNonMinBits 0x%08x (tip is min-diff, block time ≤ tip+120s)", *lastNonMinBits)
				return *lastNonMinBits
			}
			log.Printf("P2P template: WARNING — tip is min-diff, block time ≤ tip+120s, but lastNonMinBits not set; using tip.Bits=0x%08x (block may be rejected with bad-diffbits)", tip.Bits)
		}
	}

	// Pre-DigiShield: full retarget needs 240-block window timestamps (not just parent). Safe fallback: tip.Bits.
	if tip.Height < dogecoinDigishieldStartHeight {
		return tip.Bits
	}

	// DigiShield: every block from tip height ≥ 145000 (next height ≥ 145001 uses tip≥145000)
	if tipParentTimestamp == 0 {
		if tip.Height >= dogecoinDigishieldStartHeight {
			log.Printf("template: tip parent timestamp unknown — using tip.Bits 0x%08x for next work (DigiShield skipped)", tip.Bits)
		}
		return tip.Bits
	}
	return calculateDogecoinDigishieldNextWork(tip.Bits, int64(tip.Timestamp), int64(tipParentTimestamp))
}

// GetNextWorkRequired is kept for callers that do not track parent timestamp; uses extended with parent=0 (DigiShield disabled → tip.Bits).
func GetNextWorkRequired(tip *BlockHeader, blockTime int64, network string, lastNonMinBits *uint32) uint32 {
	return GetNextWorkRequiredExtended(tip, blockTime, network, lastNonMinBits, 0)
}
