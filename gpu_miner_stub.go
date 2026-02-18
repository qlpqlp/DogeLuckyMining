//go:build !windows

package main

import (
	"fmt"
	"runtime"
)

// GPUMiner stub for non-Windows: uses CPU (ScryptMiner) only. GPU/OpenCL is Windows-only in this build.
type GPUMiner struct {
	miner              *ScryptMiner
	MaxHashes          uint64
	MaxHashesThisRound uint64
	ThreadCount        int
	initialized        bool
}

func NewGPUMiner(maxHashes uint64, threadCount int) *GPUMiner {
	return &GPUMiner{
		miner:       NewScryptMinerWithConfig(maxHashes, threadCount),
		MaxHashes:   maxHashes,
		ThreadCount: threadCount,
	}
}

func (g *GPUMiner) SetNonceStrategy(searchMode, startMode string, useStride bool) {
	g.miner.SetNonceStrategy(searchMode, startMode, useStride)
}

func (g *GPUMiner) Initialize() error {
	g.initialized = true
	return nil
}

func (g *GPUMiner) MineBlock(block *Block, targetHex string) (uint32, []byte, bool, uint64) {
	g.miner.MaxHashes = g.MaxHashes
	g.miner.MaxHashesThisRound = g.MaxHashesThisRound
	g.miner.ThreadCount = g.ThreadCount
	return g.miner.MineBlock(block, targetHex)
}

func DetectGPUs() []string {
	return []string{fmt.Sprintf("CPU: %d cores (GPU/OpenCL is Windows-only in this build)", runtime.NumCPU())}
}

func IsGPUAvailable() bool {
	return false
}
