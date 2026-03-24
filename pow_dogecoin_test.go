package main

import (
	"testing"
)

func TestBigToCompactBitsToTargetRoundTrip(t *testing.T) {
	cases := []uint32{
		0x1a05e69f,
		0x1b04c1b5,
		0x1e0ffff0,
	}
	for _, bits := range cases {
		tgt := BitsToTarget(bits)
		compact := BigToCompact(tgt)
		tgt2 := BitsToTarget(compact)
		if tgt.Cmp(tgt2) != 0 {
			t.Errorf("round-trip bits 0x%08x: got 0x%08x target mismatch", bits, compact)
		}
	}
}

func TestDogecoinPowLimitUint256Loaded(t *testing.T) {
	if dogecoinPowLimitUint256 == nil || dogecoinPowLimitUint256.Sign() <= 0 {
		t.Fatal("pow limit not initialized")
	}
	// Must be at least as large as testnet compact max target interpretation
	if dogecoinPowLimitUint256.Cmp(BitsToTarget(0x1e0ffff0)) < 0 {
		t.Error("mainnet pow limit smaller than testnet compact max")
	}
}

// TestMainnetDigishieldStableWhenOneMinuteSpacing checks DigiShield leaves nBits unchanged when parent is exactly 60s before tip.
func TestMainnetDigishieldStableWhenOneMinuteSpacing(t *testing.T) {
	tip := &BlockHeader{
		Height:    5_000_000,
		Timestamp: 1_700_000_000,
		Bits:      0x1a05e69f,
	}
	parentTS := uint32(tip.Timestamp - 60)
	got := GetNextWorkRequiredExtended(tip, int64(tip.Timestamp), "mainnet", nil, parentTS)
	if got != tip.Bits {
		t.Errorf("expected unchanged bits 0x%08x, got 0x%08x", tip.Bits, got)
	}
}

// TestMainnetDigishieldFasterBlocksRaisesDifficulty: shorter inter-block time → smaller target (harder).
func TestMainnetDigishieldFasterBlocksRaisesDifficulty(t *testing.T) {
	tip := &BlockHeader{
		Height:    5_000_000,
		Timestamp: 1_700_000_000,
		Bits:      0x1a05e69f,
	}
	parentTS := uint32(tip.Timestamp - 30)
	got := GetNextWorkRequiredExtended(tip, int64(tip.Timestamp), "mainnet", nil, parentTS)
	tSlow := BitsToTarget(tip.Bits)
	tNew := BitsToTarget(got)
	if tNew.Cmp(tSlow) >= 0 {
		t.Errorf("expected smaller target after fast block (harder), slow=%v new=%v", tSlow, tNew)
	}
}

// TestMainnetPreDigishieldReturnsTipBits without parent chain walk.
func TestMainnetPreDigishieldReturnsTipBits(t *testing.T) {
	tip := &BlockHeader{Height: 144_999, Timestamp: 100, Bits: 0x1c123456}
	got := GetNextWorkRequiredExtended(tip, 200, "mainnet", nil, 99)
	if got != tip.Bits {
		t.Errorf("pre-digishield want tip bits, got 0x%08x", got)
	}
}

// TestTestnetMinDifficultyWhenStaleTime tests AllowDigishield-style min diff on testnet (height ≥ 157500).
func TestTestnetMinDifficultyWhenStaleTime(t *testing.T) {
	tip := &BlockHeader{
		Height:    200_000,
		Timestamp: 1_000_000,
		Bits:      0x1a05e69f,
	}
	blockTime := int64(tip.Timestamp) + testnetMinDifficultySpacing + 1
	got := GetNextWorkRequiredExtended(tip, blockTime, "testnet", nil, uint32(tip.Timestamp-60))
	if got != dogecoinPowLimitCompact {
		t.Errorf("expected pow limit compact, got 0x%08x", got)
	}
}

// TestTestnetMinDiffTipUsesLastNonMin when block time does not allow another min-diff block.
func TestTestnetMinDiffTipUsesLastNonMin(t *testing.T) {
	lastNM := uint32(0x1b04c1b5)
	tip := &BlockHeader{
		Height:    200_000,
		Timestamp: 1_000_000,
		Bits:      dogecoinPowLimitCompact,
	}
	got := GetNextWorkRequiredExtended(tip, int64(tip.Timestamp)+30, "testnet", &lastNM, uint32(tip.Timestamp-60))
	if got != lastNM {
		t.Errorf("expected lastNonMin 0x%08x, got 0x%08x", lastNM, got)
	}
}
