//go:build windows

package main

import (
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/crypto/scrypt"
)

//go:embed scrypt_kernel.cl
var embeddedScryptKernel []byte

var (
	openclDLL                *syscall.DLL
	clGetPlatformIDs         *syscall.Proc
	clGetDeviceIDs           *syscall.Proc
	clCreateContext          *syscall.Proc
	clCreateCommandQueue     *syscall.Proc
	clGetDeviceInfo          *syscall.Proc
	clReleaseContext         *syscall.Proc
	clReleaseCommandQueue    *syscall.Proc
	clCreateProgramWithSource *syscall.Proc
	clBuildProgram           *syscall.Proc
	clGetProgramBuildInfo    *syscall.Proc
	clCreateKernel           *syscall.Proc
	clCreateBuffer           *syscall.Proc
	clEnqueueWriteBuffer     *syscall.Proc
	clEnqueueReadBuffer      *syscall.Proc
	clEnqueueCopyBuffer      *syscall.Proc
	clSetKernelArg           *syscall.Proc
	clEnqueueNDRangeKernel   *syscall.Proc
	clFinish                 *syscall.Proc
	clReleaseProgram         *syscall.Proc
	clReleaseKernel          *syscall.Proc
	clReleaseMemObject       *syscall.Proc
	openclInitialized        bool
	openclInitMutex          sync.Mutex
)

const (
	CL_SUCCESS                = 0
	CL_DEVICE_TYPE_GPU        = 0x00000004
	CL_DEVICE_TYPE_CPU        = 0x00000002
	CL_DEVICE_NAME                 = 0x102B
	CL_DEVICE_GLOBAL_MEM_SIZE      = 0x101F
	CL_DEVICE_MAX_MEM_ALLOC_SIZE   = 0x1010
	CL_PLATFORM_NAME               = 0x0902
	CL_MEM_READ_ONLY          = 0x1
	CL_MEM_WRITE_ONLY         = 0x2
	CL_MEM_READ_WRITE         = 0x3
	CL_MEM_COPY_HOST_PTR      = 0x4
	CL_MEM_ALLOC_HOST_PTR     = 0x20
	CL_MEM_USE_HOST_PTR       = 0x40
	CL_PROGRAM_BUILD_LOG      = 0x1183
	CL_PROGRAM_BUILD_STATUS   = 0x1181
	CL_BUILD_SUCCESS          = 0
	CL_BUILD_ERROR           = -11
	CL_INVALID_KERNEL_NAME    = -46
	CL_INVALID_VALUE          = -30
)

// GPUMiner handles GPU-accelerated mining using direct OpenCL.dll calls (no CGO!)
type GPUMiner struct {
	MaxHashes         uint64
	MaxHashesThisRound uint64 // If > 0, cap this call (for testnet fast template refresh)
	ThreadCount       int
	initialized bool
	// GPU-specific settings
	openclCtx   *OpenCLContext
	// Performance optimizations
	BatchSize int // Number of nonces to process in each batch
	// Nonce search strategy settings
	NonceSearchMode string // "forward", "backward", "sequential" (default)
	NonceStartMode  string // "random", "calculated" (from block metadata)
	UseStride       bool   // Use stride-based iteration for multi-threading
	// Cancel mining when no progress for a long time (avoids hanging forever on stuck scrypt/thread)
	cancelMining atomic.Bool
}

// OpenCLContext holds OpenCL context and device information
type OpenCLContext struct {
	platforms         []uintptr
	devices          []uintptr
	context          uintptr
	commandQueue     uintptr
	device           uintptr
	program          uintptr
	kernel           uintptr
	initialized      bool
	deviceName       string
	deviceType       string // "GPU" or "CPU"
	maxScratchpadBuf       uint64 // 0 = not probed; else max bytes driver accepts for scratchpad
	useHostPtrScratchpad   bool   // if true, create scratchpad with USE_HOST_PTR
	useVirtualAllocScratch bool   // if true, use VirtualAlloc for host buffer (Windows)
	probeFailed            bool   // if true, probe already ran and no method worked; skip GPU and don't re-probe
	useSplitKernelPath     bool   // if true, use scrypt_fill + copy + scrypt_mix (no READ_WRITE buffer)
	kernelFill             uintptr
	kernelMix              uintptr
	loggedBatchOnce        bool   // avoid logging "GPU batch" every block
}

var globalOpenCLContext *OpenCLContext

// Windows page allocation for USE_HOST_PTR (some NVIDIA drivers accept only OS-allocated pages)
var (
	winKernel32     *syscall.DLL
	winVirtualAlloc *syscall.Proc
	winVirtualFree  *syscall.Proc
)
const (
	winMEM_COMMIT    = 0x1000
	winMEM_RESERVE   = 0x2000
	winMEM_RELEASE   = 0x8000
	winPAGE_READWRITE = 0x04
	winPAGE_SIZE     = 4096
)

func initWinAlloc() {
	if winVirtualAlloc != nil {
		return
	}
	dll, err := syscall.LoadDLL("kernel32.dll")
	if err != nil {
		return
	}
	winKernel32 = dll
	winVirtualAlloc, _ = dll.FindProc("VirtualAlloc")
	winVirtualFree, _ = dll.FindProc("VirtualFree")
}

// allocPageAligned allocates size bytes (rounded up to page multiple) using VirtualAlloc. Returns 0 on failure.
func allocPageAligned(size uint64) uintptr {
	initWinAlloc()
	if winVirtualAlloc == nil {
		return 0
	}
	allocSize := (size + winPAGE_SIZE - 1) / winPAGE_SIZE * winPAGE_SIZE
	ptr, _, _ := winVirtualAlloc.Call(0, uintptr(allocSize), winMEM_COMMIT|winMEM_RESERVE, winPAGE_READWRITE)
	return ptr
}

func freePageAligned(ptr uintptr) {
	if ptr == 0 || winVirtualFree == nil {
		return
	}
	winVirtualFree.Call(ptr, 0, winMEM_RELEASE)
}

// initOpenCLDLL loads OpenCL.dll and gets function pointers
func initOpenCLDLL() error {
	openclInitMutex.Lock()
	defer openclInitMutex.Unlock()

	if openclInitialized {
		return nil
	}

	// Load OpenCL.dll from System32 (comes with GPU drivers)
	dll, err := syscall.LoadDLL("OpenCL.dll")
	if err != nil {
		return fmt.Errorf("failed to load OpenCL.dll: %v (GPU drivers may not be installed)", err)
	}
	openclDLL = dll

	// Get function pointers
	clGetPlatformIDs, _ = dll.FindProc("clGetPlatformIDs")
	clGetDeviceIDs, _ = dll.FindProc("clGetDeviceIDs")
	clCreateContext, _ = dll.FindProc("clCreateContext")
	clCreateCommandQueue, _ = dll.FindProc("clCreateCommandQueue")
	clGetDeviceInfo, _ = dll.FindProc("clGetDeviceInfo")
	clReleaseContext, _ = dll.FindProc("clReleaseContext")
	clReleaseCommandQueue, _ = dll.FindProc("clReleaseCommandQueue")
	clCreateProgramWithSource, _ = dll.FindProc("clCreateProgramWithSource")
	clBuildProgram, _ = dll.FindProc("clBuildProgram")
	clGetProgramBuildInfo, _ = dll.FindProc("clGetProgramBuildInfo")
	clCreateKernel, _ = dll.FindProc("clCreateKernel")
	clCreateBuffer, _ = dll.FindProc("clCreateBuffer")
	clEnqueueWriteBuffer, _ = dll.FindProc("clEnqueueWriteBuffer")
	clEnqueueReadBuffer, _ = dll.FindProc("clEnqueueReadBuffer")
	clEnqueueCopyBuffer, _ = dll.FindProc("clEnqueueCopyBuffer")
	clSetKernelArg, _ = dll.FindProc("clSetKernelArg")
	clEnqueueNDRangeKernel, _ = dll.FindProc("clEnqueueNDRangeKernel")
	clFinish, _ = dll.FindProc("clFinish")
	clReleaseProgram, _ = dll.FindProc("clReleaseProgram")
	clReleaseKernel, _ = dll.FindProc("clReleaseKernel")
	clReleaseMemObject, _ = dll.FindProc("clReleaseMemObject")

	if clGetPlatformIDs == nil || clGetDeviceIDs == nil {
		return fmt.Errorf("failed to find OpenCL functions in OpenCL.dll")
	}

	openclInitialized = true
	return nil
}

// InitializeOpenCL initializes OpenCL using direct DLL calls (no CGO!)
func InitializeOpenCL() (*OpenCLContext, error) {
	if globalOpenCLContext != nil && globalOpenCLContext.initialized {
		return globalOpenCLContext, nil
	}

	// Initialize OpenCL DLL
	if err := initOpenCLDLL(); err != nil {
		return nil, err
	}

	ctx := &OpenCLContext{}

	// Get number of platforms
	var numPlatforms uint32
	ret, _, _ := clGetPlatformIDs.Call(0, 0, uintptr(unsafe.Pointer(&numPlatforms)))
	if ret != CL_SUCCESS || numPlatforms == 0 {
		return nil, fmt.Errorf("no OpenCL platforms found (GPU drivers may not be installed)")
	}

	// Get platforms
	platforms := make([]uintptr, numPlatforms)
	ret, _, _ = clGetPlatformIDs.Call(uintptr(numPlatforms), uintptr(unsafe.Pointer(&platforms[0])), 0)
	if ret != CL_SUCCESS {
		return nil, fmt.Errorf("failed to get OpenCL platforms: %d", ret)
	}
	ctx.platforms = platforms

	// Try to find a GPU device first
	var selectedDevice uintptr
	foundGPU := false

	for _, platform := range platforms {
		var numDevices uint32
		ret, _, _ := clGetDeviceIDs.Call(platform, CL_DEVICE_TYPE_GPU, 0, 0, uintptr(unsafe.Pointer(&numDevices)))
		if ret == CL_SUCCESS && numDevices > 0 {
			devices := make([]uintptr, numDevices)
			ret, _, _ = clGetDeviceIDs.Call(platform, CL_DEVICE_TYPE_GPU, uintptr(numDevices), uintptr(unsafe.Pointer(&devices[0])), 0)
			if ret == CL_SUCCESS && len(devices) > 0 {
				selectedDevice = devices[0]
				ctx.devices = devices
				foundGPU = true
				break
			}
		}
	}

	// Fallback to CPU if no GPU found
	if !foundGPU {
		for _, platform := range platforms {
			var numDevices uint32
			ret, _, _ := clGetDeviceIDs.Call(platform, CL_DEVICE_TYPE_CPU, 0, 0, uintptr(unsafe.Pointer(&numDevices)))
			if ret == CL_SUCCESS && numDevices > 0 {
				devices := make([]uintptr, numDevices)
				ret, _, _ = clGetDeviceIDs.Call(platform, CL_DEVICE_TYPE_CPU, uintptr(numDevices), uintptr(unsafe.Pointer(&devices[0])), 0)
				if ret == CL_SUCCESS && len(devices) > 0 {
					selectedDevice = devices[0]
					ctx.devices = devices
					log.Printf("⚠️ No GPU found, using OpenCL CPU device")
					break
				}
			}
		}
	}

	if selectedDevice == 0 {
		return nil, fmt.Errorf("no OpenCL devices found")
	}

	// Get device name
	var deviceName [128]byte
	var nameSize uint64
	ret, _, _ = clGetDeviceInfo.Call(selectedDevice, CL_DEVICE_NAME, 128, uintptr(unsafe.Pointer(&deviceName[0])), uintptr(unsafe.Pointer(&nameSize)))
	if ret == CL_SUCCESS && nameSize > 0 {
		ctx.deviceName = string(deviceName[:nameSize-1]) // Remove null terminator
	} else {
		ctx.deviceName = "Unknown Device"
	}

	// Create OpenCL context
	var errCode int32
	ctx.context, _, _ = clCreateContext.Call(0, 1, uintptr(unsafe.Pointer(&selectedDevice)), 0, 0, uintptr(unsafe.Pointer(&errCode)))
	if errCode != CL_SUCCESS || ctx.context == 0 {
		return nil, fmt.Errorf("failed to create OpenCL context: %d", errCode)
	}

	// Create command queue
	ctx.commandQueue, _, _ = clCreateCommandQueue.Call(ctx.context, selectedDevice, 0, uintptr(unsafe.Pointer(&errCode)))
	if errCode != CL_SUCCESS || ctx.commandQueue == 0 {
		clReleaseContext.Call(ctx.context)
		return nil, fmt.Errorf("failed to create command queue: %d", errCode)
	}

	ctx.device = selectedDevice
	ctx.initialized = true
	ctx.deviceType = "GPU"
	if !foundGPU {
		ctx.deviceType = "CPU"
	}

	// Build OpenCL program with Scrypt kernel
	if err := ctx.buildScryptProgram(); err != nil {
		log.Printf("⚠️ Failed to build OpenCL Scrypt program: %v", err)
		log.Printf("💡 Will use optimized CPU fallback")
		// Continue anyway - we can still use CPU fallback
	}

	globalOpenCLContext = ctx

	log.Printf("✅ OpenCL initialized on %s: %s (direct OpenCL.dll, no CGO)", ctx.deviceType, ctx.deviceName)
	return ctx, nil
}

// buildScryptProgram compiles the OpenCL Scrypt kernel (full RFC 7914 from scrypt_kernel.cl)
func (ctx *OpenCLContext) buildScryptProgram() error {
	kernelSource := string(embeddedScryptKernel)
	if len(kernelSource) == 0 {
		return fmt.Errorf("embedded scrypt kernel is empty")
	}

	// Create program from source
	kernelBytes := []byte(kernelSource)
	if len(kernelBytes) < 2 || (kernelBytes[0] != '#' && kernelBytes[0] != '/') {
		return fmt.Errorf("kernel source appears corrupted (first byte: 0x%02x)", kernelBytes[0])
	}
	
	// OpenCL expects: const char **strings, const size_t *lengths
	// Create array of char* pointers (one element)
	sourcePtr := uintptr(unsafe.Pointer(&kernelBytes[0]))
	sourcePtrArray := [1]uintptr{sourcePtr}
	
	// Create array of lengths (one element) - use size_t (uintptr on 64-bit)
	sourceLen := uintptr(len(kernelBytes))
	lengthArray := [1]uintptr{sourceLen}
	
	var errCode int32
	
	ctx.program, _, _ = clCreateProgramWithSource.Call(
		ctx.context,
		1, // count
		uintptr(unsafe.Pointer(&sourcePtrArray[0])), // const char **strings
		uintptr(unsafe.Pointer(&lengthArray[0])),    // const size_t *lengths
		uintptr(unsafe.Pointer(&errCode)),
	)
	
	if errCode != CL_SUCCESS || ctx.program == 0 {
		return fmt.Errorf("failed to create OpenCL program: %d", errCode)
	}
	
	// Build program with performance options (5–20% gain on many GPUs)
	buildOpts := []byte("-cl-fast-relaxed-math -cl-mad-enable -cl-no-signed-zeros -w\x00")
	optsPtr := uintptr(unsafe.Pointer(&buildOpts[0]))
	ret, _, _ := clBuildProgram.Call(
		ctx.program,
		1,
		uintptr(unsafe.Pointer(&ctx.device)),
		optsPtr,
		0, // callback
		0, // user_data
	)
	
	// Get build log only when build failed (NVIDIA still emits noinline warnings in log even with -w)
	var logSize uint64
	clGetProgramBuildInfo.Call(
		ctx.program,
		ctx.device,
		CL_PROGRAM_BUILD_LOG,
		0,
		0,
		uintptr(unsafe.Pointer(&logSize)),
	)
	
	if ret != CL_SUCCESS {
		if logSize > 0 && logSize < 10000 {
			logBuf := make([]byte, logSize)
			clGetProgramBuildInfo.Call(
				ctx.program,
				ctx.device,
				CL_PROGRAM_BUILD_LOG,
				uintptr(logSize),
				uintptr(unsafe.Pointer(&logBuf[0])),
				0,
			)
			buildLog := string(logBuf)
			if len(buildLog) > 0 && buildLog != "\x00" {
				log.Printf("OpenCL build log: %s", buildLog)
			}
		}
		return fmt.Errorf("failed to build OpenCL program: %d", ret)
	}
	
	// Check build status
	var buildStatus int32
	var statusSize uint64
	clGetProgramBuildInfo.Call(
		ctx.program,
		ctx.device,
		CL_PROGRAM_BUILD_STATUS,
		4,
		uintptr(unsafe.Pointer(&buildStatus)),
		uintptr(unsafe.Pointer(&statusSize)),
	)
	
	if buildStatus != CL_BUILD_SUCCESS {
		if logSize > 0 && logSize < 10000 {
			logBuf := make([]byte, logSize)
			clGetProgramBuildInfo.Call(
				ctx.program,
				ctx.device,
				CL_PROGRAM_BUILD_LOG,
				uintptr(logSize),
				uintptr(unsafe.Pointer(&logBuf[0])),
				0,
			)
			buildLog := string(logBuf)
			if len(buildLog) > 0 && buildLog != "\x00" {
				log.Printf("OpenCL build log: %s", buildLog)
			}
		}
		return fmt.Errorf("OpenCL program build status: %d (not successful)", buildStatus)
	}
	
	// Create kernel - kernel name must be null-terminated C string
	// Go strings are already null-terminated when converted to C strings
	kernelNameCStr := "mine_scrypt"
	kernelNameBytes := []byte(kernelNameCStr)
	kernelNameBytes = append(kernelNameBytes, 0) // Ensure null terminator
	kernelNamePtr := uintptr(unsafe.Pointer(&kernelNameBytes[0]))
	
	ctx.kernel, _, _ = clCreateKernel.Call(
		ctx.program,
		kernelNamePtr,
		uintptr(unsafe.Pointer(&errCode)),
	)
	
	if errCode != CL_SUCCESS || ctx.kernel == 0 {
		// Error -46 is CL_INVALID_KERNEL_NAME - kernel doesn't exist in program
		if errCode == CL_INVALID_KERNEL_NAME {
			log.Printf("⚠️ Kernel 'mine_scrypt' not found in compiled program")
			log.Printf("💡 This usually means the program compiled but the kernel function wasn't recognized")
			log.Printf("💡 Check the build log above for compilation errors or warnings")
		} else {
			log.Printf("⚠️ Failed to create kernel 'mine_scrypt' (error: %d)", errCode)
		}
		return fmt.Errorf("failed to create OpenCL kernel: %d", errCode)
	}
	
	// Create split-kernel path kernels (used when driver rejects READ_WRITE)
	for _, name := range []string{"scrypt_fill", "scrypt_mix"} {
		kb := append([]byte(name), 0)
		kp := uintptr(unsafe.Pointer(&kb[0]))
		k, _, _ := clCreateKernel.Call(ctx.program, kp, uintptr(unsafe.Pointer(&errCode)))
		if errCode == CL_SUCCESS && k != 0 {
			if name == "scrypt_fill" {
				ctx.kernelFill = k
			} else {
				ctx.kernelMix = k
			}
		}
	}
	return nil
}

// CleanupOpenCL releases OpenCL resources
func (ctx *OpenCLContext) CleanupOpenCL() {
	if ctx == nil || !ctx.initialized {
		return
	}

	if ctx.kernelFill != 0 {
		clReleaseKernel.Call(ctx.kernelFill)
		ctx.kernelFill = 0
	}
	if ctx.kernelMix != 0 {
		clReleaseKernel.Call(ctx.kernelMix)
		ctx.kernelMix = 0
	}
	if ctx.kernel != 0 {
		clReleaseKernel.Call(ctx.kernel)
		ctx.kernel = 0
	}
	if ctx.program != 0 {
		clReleaseProgram.Call(ctx.program)
		ctx.program = 0
	}
	if ctx.commandQueue != 0 {
		clReleaseCommandQueue.Call(ctx.commandQueue)
		ctx.commandQueue = 0
	}
	if ctx.context != 0 {
		clReleaseContext.Call(ctx.context)
		ctx.context = 0
	}

	ctx.initialized = false
}

// GetDeviceInfo returns information about the OpenCL device
func (ctx *OpenCLContext) GetDeviceInfo() string {
	if ctx == nil || !ctx.initialized {
		return "No OpenCL device"
	}
	return fmt.Sprintf("%s: %s", ctx.deviceType, ctx.deviceName)
}

// NewGPUMiner creates a new GPU miner instance
// In GPU mode, maximize parallelization by using all available CPU cores
func NewGPUMiner(maxHashes uint64, threadCount int) *GPUMiner {
	if threadCount <= 0 {
		// GPU mode: use ALL available CPU cores for maximum parallelization
		threadCount = runtime.NumCPU()
		// For high core count systems, use all cores
		if threadCount > 64 {
			log.Printf("🚀 GPU Mode: Using all %d CPU cores for parallel mining", threadCount)
		}
	}
	// Allow up to 128 threads for high-end CPUs
	if threadCount > 128 {
		threadCount = 128
	}

	// Optimize batch size based on thread count for maximum throughput
	batchSize := 262144
	if threadCount > 8 {
		batchSize = 262144 * 2
	}
	if threadCount > 16 {
		batchSize = 262144 * 3
	}
	if threadCount > 32 {
		batchSize = 262144 * 4
	}

	return &GPUMiner{
		MaxHashes:       maxHashes,
		ThreadCount:     threadCount,
		BatchSize:       batchSize,
		NonceSearchMode: "sequential", // Default: sequential from 0
		NonceStartMode:  "calculated", // Default: calculate from block metadata
		UseStride:       true,          // Use stride for better thread distribution
	}
}

// SetNonceStrategy configures nonce search strategy
func (g *GPUMiner) SetNonceStrategy(searchMode, startMode string, useStride bool) {
	g.NonceSearchMode = searchMode
	g.NonceStartMode = startMode
	g.UseStride = useStride
}

// calculateStartNonce computes starting nonce from block metadata
// Uses: Version XOR Timestamp XOR Bits (deterministic per block)
func calculateStartNonce(block *Block, startMode string) uint32 {
	if startMode == "random" {
		// Use current time as seed for randomness
		// Note: This is pseudo-random, not cryptographically secure
		// For true randomness, we'd need crypto/rand, but that's slow
		return uint32(time.Now().UnixNano() & 0xFFFFFFFF)
	}
	
	// "calculated" mode: Use block metadata
	// XOR Version, Timestamp, and Bits for deterministic starting point
	startNonce := uint32(block.Version) ^ block.Timestamp ^ block.Bits
	
	// Also XOR with merkle root bytes for more entropy
	for i := 0; i < 4 && i < len(block.MerkleRoot); i++ {
		startNonce ^= uint32(block.MerkleRoot[i]) << (i * 8)
	}
	
	return startNonce
}

// Initialize attempts to initialize GPU support using direct OpenCL.dll calls
func (g *GPUMiner) Initialize() error {
	// Try to initialize OpenCL (direct DLL calls, no CGO!)
	openclCtx, err := InitializeOpenCL()
	if err != nil {
		// OpenCL not available - use optimized parallel CPU threads
		log.Printf("💻 GPU Mode: Using %d CPU threads for massively parallel mining", g.ThreadCount)
		log.Printf("ℹ️  OpenCL error: %v", err)
		log.Printf("ℹ️  This is normal if you don't have GPU drivers installed")
		log.Printf("✨ CPU parallel mining works great - will use all %d cores!", g.ThreadCount)
	} else {
		// OpenCL initialized successfully!
		g.openclCtx = openclCtx
		log.Printf("⚡ GPU will be used for Scrypt mining (fallback: %d CPU threads if needed)", g.ThreadCount)
	}
	
	// Mark as initialized (either GPU or CPU)
	g.initialized = true
	return nil
}

func (g *GPUMiner) effectiveMaxHashes() uint64 {
	limit := g.MaxHashes
	if g.MaxHashesThisRound > 0 && g.MaxHashesThisRound < limit {
		limit = g.MaxHashesThisRound
	}
	return limit
}

// probeScratchpadBuffer finds the largest scratchpad size and creation method the driver accepts (once per context).
func (ctx *OpenCLContext) probeScratchpadBuffer() {
	if ctx.probeFailed {
		return
	}
	if ctx.maxScratchpadBuf != 0 {
		return
	}
	const scratchpadPerThread = 1024 * 32 * 4
	sizes := []uint64{128 * 1024, 1024 * 1024, 4 * 1024 * 1024, 16 * 1024 * 1024, 32 * 1024 * 1024, 64 * 1024 * 1024, 128 * 1024 * 1024, 256 * 1024 * 1024}
	var errCode int32
	tryCreate := func(flags uintptr, size uint64, hostPtr uintptr) bool {
		buf, _, _ := clCreateBuffer.Call(ctx.context, flags, uintptr(size), hostPtr, uintptr(unsafe.Pointer(&errCode)))
		if errCode == CL_SUCCESS && buf != 0 {
			clReleaseMemObject.Call(buf)
			return true
		}
		return false
	}
	// 1) Try device buffer CL_MEM_READ_WRITE
	for i := len(sizes) - 1; i >= 0; i-- {
		if tryCreate(uintptr(CL_MEM_READ_WRITE), sizes[i], 0) {
			ctx.maxScratchpadBuf = sizes[i]
			ctx.useHostPtrScratchpad = false
			log.Printf("ℹ️ GPU scratchpad: device buffer OK up to %d MB", ctx.maxScratchpadBuf/(1024*1024))
			return
		}
	}
	// 2) Try CL_MEM_READ_WRITE | CL_MEM_ALLOC_HOST_PTR
	for i := len(sizes) - 1; i >= 0; i-- {
		if tryCreate(uintptr(CL_MEM_READ_WRITE|CL_MEM_ALLOC_HOST_PTR), sizes[i], 0) {
			ctx.maxScratchpadBuf = sizes[i]
			ctx.useHostPtrScratchpad = false
			log.Printf("ℹ️ GPU scratchpad: ALLOC_HOST_PTR OK up to %d MB", ctx.maxScratchpadBuf/(1024*1024))
			return
		}
	}
	// 3) Try USE_HOST_PTR with page-aligned Go slice
	const align = 4096
	for i := len(sizes) - 1; i >= 0; i-- {
		size := sizes[i]
		hostBuf := make([]byte, size+align)
		base := uintptr(unsafe.Pointer(&hostBuf[0]))
		offset := (align - base%align) % align
		alignedPtr := base + offset
		if tryCreate(uintptr(CL_MEM_READ_WRITE|CL_MEM_USE_HOST_PTR), size, alignedPtr) {
			ctx.maxScratchpadBuf = size
			ctx.useHostPtrScratchpad = true
			ctx.useVirtualAllocScratch = false
			runtime.KeepAlive(hostBuf)
			log.Printf("ℹ️ GPU scratchpad: USE_HOST_PTR (aligned) OK up to %d MB", ctx.maxScratchpadBuf/(1024*1024))
			return
		}
	}
	// 4) Try USE_HOST_PTR with VirtualAlloc (Windows: some drivers accept only OS-allocated pages)
	for i := len(sizes) - 1; i >= 0; i-- {
		size := sizes[i]
		ptr := allocPageAligned(size)
		if ptr == 0 {
			continue
		}
		if tryCreate(uintptr(CL_MEM_READ_WRITE|CL_MEM_USE_HOST_PTR), size, ptr) {
			freePageAligned(ptr)
			ctx.maxScratchpadBuf = size
			ctx.useHostPtrScratchpad = true
			ctx.useVirtualAllocScratch = true
			log.Printf("ℹ️ GPU scratchpad: USE_HOST_PTR (VirtualAlloc) OK up to %d MB", ctx.maxScratchpadBuf/(1024*1024))
			return
		}
		freePageAligned(ptr)
	}
	// 5) Split-kernel path: use only WRITE_ONLY and READ_ONLY (no READ_WRITE). Works on drivers that reject READ_WRITE.
	if ctx.kernelFill != 0 && ctx.kernelMix != 0 && clEnqueueCopyBuffer != nil {
		for i := len(sizes) - 1; i >= 0; i-- {
			size := sizes[i]
			vw, _, _ := clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_WRITE_ONLY), uintptr(size), 0, uintptr(unsafe.Pointer(&errCode)))
			if errCode != CL_SUCCESS || vw == 0 {
				continue
			}
			vr, _, _ := clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_READ_ONLY), uintptr(size), 0, uintptr(unsafe.Pointer(&errCode)))
			clReleaseMemObject.Call(vw)
			if errCode != CL_SUCCESS || vr == 0 {
				continue
			}
			clReleaseMemObject.Call(vr)
			ctx.maxScratchpadBuf = size
			ctx.useSplitKernelPath = true
			log.Printf("ℹ️ GPU scratchpad: split-kernel path (WRITE_ONLY+READ_ONLY) OK up to %d MB", ctx.maxScratchpadBuf/(1024*1024))
			return
		}
	}
	ctx.probeFailed = true
	if tryCreate(uintptr(CL_MEM_READ_WRITE), 256, 0) {
		log.Printf("⚠️ GPU scratchpad: no large buffer worked (256-byte OK); driver may limit READ_WRITE size. Using CPU mining.")
	} else {
		log.Printf("⚠️ GPU scratchpad: driver rejected READ_WRITE (err %d). Using CPU mining.", errCode)
	}
}

// MineBlock mines a block using GPU acceleration (or parallel CPU as fallback)
func (g *GPUMiner) MineBlock(block *Block, targetHex string) (uint32, []byte, bool, uint64) {
	if !g.initialized {
		if err := g.Initialize(); err != nil {
			return 0, nil, false, 0
		}
	}

	// Convert target from hex string to big.Int
	targetBytes, err := hex.DecodeString(targetHex)
	if err != nil {
		return 0, nil, false, 0
	}

	targetBig := new(big.Int).SetBytes(targetBytes)
	if targetBig.Cmp(big.NewInt(0)) == 0 {
		targetBig = big.NewInt(1)
	}

	// Use real GPU acceleration if available, otherwise fall back to parallel CPU
	if g.openclCtx != nil && g.openclCtx.initialized {
		return g.mineWithOpenCL(block, targetBig)
	}

	// Fallback to optimized parallel CPU mining
	return g.mineParallel(block, targetBig)
}

// mineWithOpenCL: Attempts OpenCL GPU mining, falls back to CPU only when GPU did not run (e.g. probe failed).
func (g *GPUMiner) mineWithOpenCL(block *Block, targetBig *big.Int) (uint32, []byte, bool, uint64) {
	if g.openclCtx != nil && g.openclCtx.initialized {
		nonce, hash, found, hashes := g.mineWithOpenCLKernel(block, targetBig)
		if found {
			return nonce, hash, found, hashes
		}
		// GPU ran and completed a round (hashes > 0); don't run CPU as well — return and fetch new template
		if hashes > 0 {
			return 0, nil, false, hashes
		}
	}
	// GPU unavailable or didn't run; use CPU
	return g.mineParallel(block, targetBig)
}

func (g *GPUMiner) mineWithOpenCLKernel(block *Block, targetBig *big.Int) (uint32, []byte, bool, uint64) {
	if g.openclCtx == nil || !g.openclCtx.initialized {
		return g.mineParallel(block, targetBig)
	}
	// Need either single kernel (mine_scrypt) or split kernels (scrypt_fill + scrypt_mix)
	if g.openclCtx.kernel == 0 && !g.openclCtx.useSplitKernelPath {
		return g.mineParallel(block, targetBig)
	}

	ctx := g.openclCtx
	ctx.probeScratchpadBuffer()
	if ctx.maxScratchpadBuf == 0 {
		return g.mineParallel(block, targetBig)
	}

	// Pre-compute header base (everything except nonce)
	// This is the optimization: we pre-compute 76 bytes (Version + PrevBlock + MerkleRoot + Timestamp + Bits)
	// and only update the 4-byte nonce for each hash attempt
	headerBase := g.precomputeHeaderBase(block)
	
	// Validate header base size (should be 76 bytes: 80 - 4 for nonce)
	if len(headerBase) == 0 || len(headerBase) != 76 {
		log.Printf("⚠️ Invalid header base size: %d (expected 76)", len(headerBase))
		return g.mineParallel(block, targetBig)
	}
	
	// Target in LE for Dogecoin PoW (Core uses uint256 LE)
	targetBytes := make([]byte, 32)
	targetBigBytes := targetBig.Bytes()
	copy(targetBytes[32-len(targetBigBytes):], targetBigBytes)
	reverseBytesInPlace(targetBytes)
	
	maxHashes := g.effectiveMaxHashes()
	// Scrypt needs 128KB scratchpad per work item (1024*32 uints). Limit batch by VRAM.
	const scratchpadPerThread = 1024 * 32 * 4 // 128KB in bytes
	const maxThreadsPerBatch = 1024            // default cap; may be reduced by VRAM
	var globalMem uint64
	clGetDeviceInfo.Call(ctx.device, CL_DEVICE_GLOBAL_MEM_SIZE, 8, uintptr(unsafe.Pointer(&globalMem)), 0)
	var maxAllocSize uint64
	clGetDeviceInfo.Call(ctx.device, CL_DEVICE_MAX_MEM_ALLOC_SIZE, 8, uintptr(unsafe.Pointer(&maxAllocSize)), 0)
	maxByVRAM := uint64(0)
	if globalMem > scratchpadPerThread {
		// Use at most 1/4 of VRAM for scratchpad to leave room for other buffers and OS
		maxByVRAM = (globalMem / 4) / scratchpadPerThread
		if maxByVRAM > 256 {
			maxByVRAM = (maxByVRAM / 256) * 256
		}
	}
	// OpenCL forbids single buffers larger than CL_DEVICE_MAX_MEM_ALLOC_SIZE (avoids -30 CL_INVALID_VALUE)
	maxByAlloc := uint64(0)
	if maxAllocSize > scratchpadPerThread {
		maxByAlloc = maxAllocSize / scratchpadPerThread
		if maxByAlloc > 256 {
			maxByAlloc = (maxByAlloc / 256) * 256
		}
	}
	batchSize := uint64(maxThreadsPerBatch)
	if maxByVRAM > 0 && batchSize > maxByVRAM {
		batchSize = maxByVRAM
	}
	if maxByAlloc > 0 && batchSize > maxByAlloc {
		batchSize = maxByAlloc
		if batchSize < 256 {
			log.Printf("ℹ️ GPU max single allocation limits batch to %d threads (scratchpad %d MB); using 256", batchSize, (256*scratchpadPerThread)/(1024*1024))
			batchSize = 256
		}
	}
	if ctx.maxScratchpadBuf > 0 && (batchSize*scratchpadPerThread) > ctx.maxScratchpadBuf {
		batchSize = ctx.maxScratchpadBuf / scratchpadPerThread
		if batchSize > 256 {
			batchSize = (batchSize / 256) * 256
		}
		if batchSize < 256 {
			batchSize = 256
		}
	}
	if batchSize > maxHashes {
		batchSize = maxHashes
	}
	if batchSize > 256 {
		batchSize = (batchSize / 256) * 256
	}
	if batchSize == 0 {
		batchSize = 256
	}
	if batchSize == 0 {
		log.Printf("⚠️ Batch size is zero")
		return g.mineParallel(block, targetBig)
	}
	if !ctx.loggedBatchOnce {
		scratchpadMB := (batchSize * scratchpadPerThread) / (1024 * 1024)
		log.Printf("ℹ️ GPU batch: %d threads, scratchpad %d MB (max single alloc %d MB)", batchSize, scratchpadMB, maxAllocSize/(1024*1024))
		ctx.loggedBatchOnce = true
	}

	// Reusable buffers for split-kernel path (avoid create/release every batch)
	var reuseHeaderBuf, reuseTargetBuf, reuseResultsBuf, reuseV_write, reuseV_read, reuseState_write, reuseState_read uintptr
	if ctx.useSplitKernelPath && ctx.kernelFill != 0 && ctx.kernelMix != 0 && clEnqueueCopyBuffer != nil {
		var errCode int32
		reuseHeaderBuf, _, _ = clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_READ_ONLY), uintptr(len(headerBase)), 0, uintptr(unsafe.Pointer(&errCode)))
		if errCode == CL_SUCCESS && reuseHeaderBuf != 0 {
			reuseTargetBuf, _, _ = clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_READ_ONLY), 32, 0, uintptr(unsafe.Pointer(&errCode)))
		}
		if errCode == CL_SUCCESS && reuseTargetBuf != 0 {
			reuseResultsBuf, _, _ = clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_WRITE_ONLY), 34*4, 0, uintptr(unsafe.Pointer(&errCode)))
		}
		if errCode == CL_SUCCESS && reuseResultsBuf != 0 {
			scratchpadSize := batchSize * scratchpadPerThread
			reuseV_write, _, _ = clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_WRITE_ONLY), uintptr(scratchpadSize), 0, uintptr(unsafe.Pointer(&errCode)))
		}
		if errCode == CL_SUCCESS && reuseV_write != 0 {
			scratchpadSize := batchSize * scratchpadPerThread
			reuseV_read, _, _ = clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_READ_ONLY), uintptr(scratchpadSize), 0, uintptr(unsafe.Pointer(&errCode)))
		}
		if errCode == CL_SUCCESS && reuseV_read != 0 {
			stateSize := batchSize * 32 * 4
			reuseState_write, _, _ = clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_WRITE_ONLY), uintptr(stateSize), 0, uintptr(unsafe.Pointer(&errCode)))
		}
		if errCode == CL_SUCCESS && reuseState_write != 0 {
			reuseStateReadSize := batchSize * 32 * 4
			reuseState_read, _, _ = clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_READ_ONLY), uintptr(reuseStateReadSize), 0, uintptr(unsafe.Pointer(&errCode)))
		}
		if errCode != CL_SUCCESS || reuseState_read == 0 {
			if reuseHeaderBuf != 0 {
				clReleaseMemObject.Call(reuseHeaderBuf)
			}
			if reuseTargetBuf != 0 {
				clReleaseMemObject.Call(reuseTargetBuf)
			}
			if reuseResultsBuf != 0 {
				clReleaseMemObject.Call(reuseResultsBuf)
			}
			if reuseV_write != 0 {
				clReleaseMemObject.Call(reuseV_write)
			}
			if reuseV_read != 0 {
				clReleaseMemObject.Call(reuseV_read)
			}
			if reuseState_write != 0 {
				clReleaseMemObject.Call(reuseState_write)
			}
			reuseHeaderBuf, reuseTargetBuf, reuseResultsBuf, reuseV_write, reuseV_read, reuseState_write, reuseState_read = 0, 0, 0, 0, 0, 0, 0
		} else {
			clEnqueueWriteBuffer.Call(ctx.commandQueue, reuseHeaderBuf, 1, 0, uintptr(len(headerBase)), uintptr(unsafe.Pointer(&headerBase[0])), 0, 0, 0)
			clEnqueueWriteBuffer.Call(ctx.commandQueue, reuseTargetBuf, 1, 0, 32, uintptr(unsafe.Pointer(&targetBytes[0])), 0, 0, 0)
			defer func() {
				clReleaseMemObject.Call(reuseHeaderBuf)
				clReleaseMemObject.Call(reuseTargetBuf)
				clReleaseMemObject.Call(reuseResultsBuf)
				clReleaseMemObject.Call(reuseV_write)
				clReleaseMemObject.Call(reuseV_read)
				clReleaseMemObject.Call(reuseState_write)
				clReleaseMemObject.Call(reuseState_read)
			}()
		}
	}

	// Use a random starting nonce each round so we don't always search only 0..99k (winning nonce can be anywhere in 0..2^32-1).
	startNonce := uint32(rand.Uint32())
	totalHashesAttempted := uint64(0)
	found := false

	for totalHashesAttempted < maxHashes && !found {
		var hostScratchpad []byte   // used when USE_HOST_PTR with Go slice
		var hostScratchpadPtr uintptr // used when USE_HOST_PTR with VirtualAlloc; free after release
		remainingHashes := maxHashes - totalHashesAttempted
		workSize := batchSize
		if remainingHashes < batchSize {
			workSize = remainingHashes
		}
		if workSize > 256 {
			workSize = (workSize / 256) * 256
		} else if workSize > 0 && workSize < 256 {
			if workSize > 128 {
				workSize = 128
			} else if workSize > 64 {
				workSize = 64
			} else if workSize > 32 {
				workSize = 32
			} else {
				workSize = 32
			}
		}
		if workSize == 0 {
			break
		}

		nonceStart := startNonce + uint32(totalHashesAttempted)

		var errCode int32
		var ret uintptr
		var headerBaseBuf, targetBuf, resultsBuf uintptr
		usingReuseBuffers := ctx.useSplitKernelPath && reuseHeaderBuf != 0

		if !usingReuseBuffers {
			// Header base buffer (76 bytes)
			headerBaseBuf, _, _ = clCreateBuffer.Call(
				ctx.context,
				CL_MEM_READ_ONLY,
				uintptr(len(headerBase)),
				0,
				uintptr(unsafe.Pointer(&errCode)),
			)
			if errCode != CL_SUCCESS || headerBaseBuf == 0 {
				log.Printf("⚠️ Failed to create header buffer: %d", errCode)
				totalHashesAttempted += workSize
				continue
			}
			ret, _, _ := clEnqueueWriteBuffer.Call(
				ctx.commandQueue,
				headerBaseBuf,
				1, 0, uintptr(len(headerBase)),
				uintptr(unsafe.Pointer(&headerBase[0])),
				0, 0, 0,
			)
			if ret != CL_SUCCESS {
				clReleaseMemObject.Call(headerBaseBuf)
				totalHashesAttempted += workSize
				continue
			}

			// Target buffer (32 bytes)
			targetBuf, _, _ = clCreateBuffer.Call(
				ctx.context,
				CL_MEM_READ_ONLY,
				32, 0,
				uintptr(unsafe.Pointer(&errCode)),
			)
			if errCode != CL_SUCCESS || targetBuf == 0 {
				clReleaseMemObject.Call(headerBaseBuf)
				totalHashesAttempted += workSize
				continue
			}
			ret, _, _ = clEnqueueWriteBuffer.Call(
				ctx.commandQueue,
				targetBuf,
				1, 0, 32,
				uintptr(unsafe.Pointer(&targetBytes[0])),
				0, 0, 0,
			)
			if ret != CL_SUCCESS {
				clReleaseMemObject.Call(headerBaseBuf)
				clReleaseMemObject.Call(targetBuf)
				totalHashesAttempted += workSize
				continue
			}

			// Results buffer: kernel writes results[0]=found, results[1]=nonce, results[2..33]=hash (34 uints = 136 bytes)
			resultsBufSize := 34 * 4
			resultsBuf, _, _ = clCreateBuffer.Call(
				ctx.context,
				CL_MEM_WRITE_ONLY,
				uintptr(resultsBufSize),
				0,
				uintptr(unsafe.Pointer(&errCode)),
			)
			if errCode != CL_SUCCESS || resultsBuf == 0 {
				clReleaseMemObject.Call(headerBaseBuf)
				clReleaseMemObject.Call(targetBuf)
				totalHashesAttempted += workSize
				continue
			}
		} else {
			headerBaseBuf = reuseHeaderBuf
			targetBuf = reuseTargetBuf
			resultsBuf = reuseResultsBuf
		}

		// Split-kernel path: only WRITE_ONLY and READ_ONLY buffers (works when driver rejects READ_WRITE)
		if ctx.useSplitKernelPath && ctx.kernelFill != 0 && ctx.kernelMix != 0 && clEnqueueCopyBuffer != nil {
			scratchpadSize := workSize * scratchpadPerThread
			stateSize := workSize * 32 * 4 // 32 uints per work item
			V_write := reuseV_write
			V_read := reuseV_read
			state_write := reuseState_write
			state_read := reuseState_read
			if !usingReuseBuffers {
				V_write, _, _ = clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_WRITE_ONLY), uintptr(scratchpadSize), 0, uintptr(unsafe.Pointer(&errCode)))
				if errCode != CL_SUCCESS || V_write == 0 {
					if !usingReuseBuffers {
						clReleaseMemObject.Call(headerBaseBuf)
						clReleaseMemObject.Call(targetBuf)
						clReleaseMemObject.Call(resultsBuf)
					}
					totalHashesAttempted += workSize
					continue
				}
				V_read, _, _ = clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_READ_ONLY), uintptr(scratchpadSize), 0, uintptr(unsafe.Pointer(&errCode)))
				if errCode != CL_SUCCESS || V_read == 0 {
					if !usingReuseBuffers {
						clReleaseMemObject.Call(headerBaseBuf)
						clReleaseMemObject.Call(targetBuf)
						clReleaseMemObject.Call(resultsBuf)
					}
					clReleaseMemObject.Call(V_write)
					totalHashesAttempted += workSize
					continue
				}
				state_write, _, _ = clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_WRITE_ONLY), uintptr(stateSize), 0, uintptr(unsafe.Pointer(&errCode)))
				if errCode != CL_SUCCESS || state_write == 0 {
					if !usingReuseBuffers {
						clReleaseMemObject.Call(headerBaseBuf)
						clReleaseMemObject.Call(targetBuf)
						clReleaseMemObject.Call(resultsBuf)
					}
					clReleaseMemObject.Call(V_write)
					clReleaseMemObject.Call(V_read)
					totalHashesAttempted += workSize
					continue
				}
				state_read, _, _ = clCreateBuffer.Call(ctx.context, uintptr(CL_MEM_READ_ONLY), uintptr(stateSize), 0, uintptr(unsafe.Pointer(&errCode)))
				if errCode != CL_SUCCESS || state_read == 0 {
					if !usingReuseBuffers {
						clReleaseMemObject.Call(headerBaseBuf)
						clReleaseMemObject.Call(targetBuf)
						clReleaseMemObject.Call(resultsBuf)
					}
					clReleaseMemObject.Call(V_write)
					clReleaseMemObject.Call(V_read)
					clReleaseMemObject.Call(state_write)
					totalHashesAttempted += workSize
					continue
				}
			}
			// Local work size for better occupancy (128/64 preferred for NVIDIA)
			globalWorkSize := uintptr(workSize)
			var localWorkSize uintptr
			for _, size := range []uintptr{128, 64, 256, 32, 16} {
				if workSize%uint64(size) == 0 {
					localWorkSize = size
					break
				}
			}
			var localWorkSizePtr uintptr
			if localWorkSize > 0 {
				localWorkSizePtr = uintptr(unsafe.Pointer(&localWorkSize))
			}
			clSetKernelArg.Call(ctx.kernelFill, 0, 8, uintptr(unsafe.Pointer(&headerBaseBuf)))
			clSetKernelArg.Call(ctx.kernelFill, 1, 4, uintptr(unsafe.Pointer(&nonceStart)))
			clSetKernelArg.Call(ctx.kernelFill, 2, 8, uintptr(unsafe.Pointer(&V_write)))
			clSetKernelArg.Call(ctx.kernelFill, 3, 8, uintptr(unsafe.Pointer(&state_write)))
			ret, _, _ := clEnqueueNDRangeKernel.Call(ctx.commandQueue, ctx.kernelFill, 1, 0, uintptr(unsafe.Pointer(&globalWorkSize)), localWorkSizePtr, 0, 0, 0)
			if ret != CL_SUCCESS {
				if !usingReuseBuffers {
					clReleaseMemObject.Call(headerBaseBuf)
					clReleaseMemObject.Call(targetBuf)
					clReleaseMemObject.Call(resultsBuf)
					clReleaseMemObject.Call(V_write)
					clReleaseMemObject.Call(V_read)
					clReleaseMemObject.Call(state_write)
					clReleaseMemObject.Call(state_read)
				}
				totalHashesAttempted += workSize
				continue
			}
			clEnqueueCopyBuffer.Call(ctx.commandQueue, V_write, V_read, 0, 0, uintptr(scratchpadSize), 0, 0, 0)
			clEnqueueCopyBuffer.Call(ctx.commandQueue, state_write, state_read, 0, 0, uintptr(stateSize), 0, 0, 0)
			clSetKernelArg.Call(ctx.kernelMix, 0, 8, uintptr(unsafe.Pointer(&headerBaseBuf)))
			clSetKernelArg.Call(ctx.kernelMix, 1, 4, uintptr(unsafe.Pointer(&nonceStart)))
			clSetKernelArg.Call(ctx.kernelMix, 2, 8, uintptr(unsafe.Pointer(&state_read)))
			clSetKernelArg.Call(ctx.kernelMix, 3, 8, uintptr(unsafe.Pointer(&V_read)))
			clSetKernelArg.Call(ctx.kernelMix, 4, 8, uintptr(unsafe.Pointer(&targetBuf)))
			clSetKernelArg.Call(ctx.kernelMix, 5, 8, uintptr(unsafe.Pointer(&resultsBuf)))
			ret, _, _ = clEnqueueNDRangeKernel.Call(ctx.commandQueue, ctx.kernelMix, 1, 0, uintptr(unsafe.Pointer(&globalWorkSize)), localWorkSizePtr, 0, 0, 0)
			if ret != CL_SUCCESS {
				if !usingReuseBuffers {
					clReleaseMemObject.Call(headerBaseBuf)
					clReleaseMemObject.Call(targetBuf)
					clReleaseMemObject.Call(resultsBuf)
					clReleaseMemObject.Call(V_write)
					clReleaseMemObject.Call(V_read)
					clReleaseMemObject.Call(state_write)
					clReleaseMemObject.Call(state_read)
				}
				totalHashesAttempted += workSize
				continue
			}
			clFinish.Call(ctx.commandQueue)
			resultsData := make([]uint32, 34)
			ret, _, _ = clEnqueueReadBuffer.Call(ctx.commandQueue, resultsBuf, 1, 0, uintptr(34*4), uintptr(unsafe.Pointer(&resultsData[0])), 0, 0, 0)
			if !usingReuseBuffers {
				clReleaseMemObject.Call(headerBaseBuf)
				clReleaseMemObject.Call(targetBuf)
				clReleaseMemObject.Call(resultsBuf)
				clReleaseMemObject.Call(V_write)
				clReleaseMemObject.Call(V_read)
				clReleaseMemObject.Call(state_write)
				clReleaseMemObject.Call(state_read)
			}
			batchFound := ret == CL_SUCCESS && resultsData[0] == 1
			var batchNonce uint32
			var batchHash []byte
			if batchFound {
				batchNonce = resultsData[1]
				batchHash = make([]byte, 32)
				for i := 0; i < 32; i++ {
					batchHash[i] = byte(resultsData[2+i] & 0xff)
				}
				log.Printf("🎉 GPU found block! Nonce: %d", batchNonce)
			}
			totalHashesAttempted += workSize
			if batchFound {
				return batchNonce, batchHash, true, totalHashesAttempted
			}
			continue
		}

		// Scratchpad: 128KB per work item (1024*32 uints). Use method that probe found to work.
		scratchpadSize := workSize * scratchpadPerThread
		var scratchpadBuf uintptr
		if ctx.useHostPtrScratchpad {
			var hostPtr uintptr
			if ctx.useVirtualAllocScratch {
				hostScratchpadPtr = allocPageAligned(scratchpadSize)
				if hostScratchpadPtr == 0 {
					clReleaseMemObject.Call(headerBaseBuf)
					clReleaseMemObject.Call(targetBuf)
					clReleaseMemObject.Call(resultsBuf)
					log.Printf("⚠️ VirtualAlloc failed for scratchpad (%d MB)", scratchpadSize/(1024*1024))
					totalHashesAttempted += workSize
					continue
				}
				hostPtr = hostScratchpadPtr
			} else {
				const align = 4096
				hostScratchpad = make([]byte, scratchpadSize+align)
				base := uintptr(unsafe.Pointer(&hostScratchpad[0]))
				offset := (align - base%align) % align
				hostPtr = base + offset
			}
			scratchpadBuf, _, _ = clCreateBuffer.Call(
				ctx.context,
				uintptr(CL_MEM_READ_WRITE|CL_MEM_USE_HOST_PTR),
				uintptr(scratchpadSize),
				hostPtr,
				uintptr(unsafe.Pointer(&errCode)),
			)
		} else {
			scratchpadBuf, _, _ = clCreateBuffer.Call(
				ctx.context,
				uintptr(CL_MEM_READ_WRITE),
				uintptr(scratchpadSize),
				0,
				uintptr(unsafe.Pointer(&errCode)),
			)
			if errCode != CL_SUCCESS || scratchpadBuf == 0 {
				scratchpadBuf, _, _ = clCreateBuffer.Call(
					ctx.context,
					uintptr(CL_MEM_READ_WRITE|CL_MEM_ALLOC_HOST_PTR),
					uintptr(scratchpadSize),
					0,
					uintptr(unsafe.Pointer(&errCode)),
				)
			}
		}
		if errCode != CL_SUCCESS || scratchpadBuf == 0 {
			clReleaseMemObject.Call(headerBaseBuf)
			clReleaseMemObject.Call(targetBuf)
			clReleaseMemObject.Call(resultsBuf)
			log.Printf("⚠️ Failed to create scratchpad buffer: %d (size %d MB)", errCode, scratchpadSize/(1024*1024))
			// On CL_INVALID_VALUE (-30), driver may reject size; halve batch and retry next iteration
			if errCode == CL_INVALID_VALUE && batchSize > 256 {
				batchSize = batchSize / 2
				if batchSize < 256 {
					batchSize = 256
				}
				log.Printf("ℹ️ Reducing GPU batch to %d threads (%d MB scratchpad), retrying...", batchSize, (batchSize*scratchpadPerThread)/(1024*1024))
			} else {
				totalHashesAttempted += workSize
			}
			continue
		}

		// Set kernel arguments: headerBase, nonceStart(uint), target, results, scratchpad
		clSetKernelArg.Call(ctx.kernel, 0, 8, uintptr(unsafe.Pointer(&headerBaseBuf)))
		clSetKernelArg.Call(ctx.kernel, 1, 4, uintptr(unsafe.Pointer(&nonceStart)))
		clSetKernelArg.Call(ctx.kernel, 2, 8, uintptr(unsafe.Pointer(&targetBuf)))
		clSetKernelArg.Call(ctx.kernel, 3, 8, uintptr(unsafe.Pointer(&resultsBuf)))
		clSetKernelArg.Call(ctx.kernel, 4, 8, uintptr(unsafe.Pointer(&scratchpadBuf)))
		
		// Execute kernel with adaptive work group size
		// The local work size must divide evenly into global work size
		// Try common work group sizes: 256, 128, 64, 32, or let OpenCL choose (NULL)
		globalWorkSize := uintptr(workSize)
		var localWorkSize uintptr = 0 // NULL - let OpenCL choose optimal size
		
		// Try to find a valid local work size that divides evenly (128/64 preferred for NVIDIA)
		validLocalSizes := []uintptr{128, 64, 256, 32, 16}
		for _, size := range validLocalSizes {
			if workSize%uint64(size) == 0 {
				localWorkSize = size
				break
			}
		}
		
		// If no valid size found, use NULL to let OpenCL choose
		var localWorkSizePtr uintptr
		if localWorkSize > 0 {
			localWorkSizePtr = uintptr(unsafe.Pointer(&localWorkSize))
		} else {
			localWorkSizePtr = 0 // NULL - let OpenCL choose
		}
		
		ret, _, _ = clEnqueueNDRangeKernel.Call(
			ctx.commandQueue,
			ctx.kernel,
			1,
			0,
			uintptr(unsafe.Pointer(&globalWorkSize)),
			localWorkSizePtr,
			0,
			0,
			0,
		)
		
		if ret != CL_SUCCESS {
			errCode := int32(ret)
			if errCode < 0 {
				errCode = int32(uint32(ret))
			}
			log.Printf("⚠️ Failed to enqueue kernel: %d (CL error code)", errCode)
			if errCode == -54 {
				log.Printf("💡 CL_INVALID_WORK_GROUP_SIZE: Work size %d may not be compatible with GPU. Falling back to CPU mining for this batch.", workSize)
				clReleaseMemObject.Call(headerBaseBuf)
				clReleaseMemObject.Call(targetBuf)
				clReleaseMemObject.Call(resultsBuf)
				runtime.KeepAlive(hostScratchpad)
				clReleaseMemObject.Call(scratchpadBuf)
				if ctx.useVirtualAllocScratch && hostScratchpadPtr != 0 {
					freePageAligned(hostScratchpadPtr)
					hostScratchpadPtr = 0
				}
				remainingHashes := maxHashes - totalHashesAttempted
				if remainingHashes > 0 {
					cpuNonce, cpuHash, cpuFound, cpuHashes := g.mineParallel(block, targetBig)
					totalHashesAttempted += cpuHashes
					if cpuFound {
						return cpuNonce, cpuHash, true, totalHashesAttempted
					}
				}
				break
			}
			clReleaseMemObject.Call(headerBaseBuf)
			clReleaseMemObject.Call(targetBuf)
			clReleaseMemObject.Call(resultsBuf)
			runtime.KeepAlive(hostScratchpad)
			clReleaseMemObject.Call(scratchpadBuf)
			if ctx.useVirtualAllocScratch && hostScratchpadPtr != 0 {
				freePageAligned(hostScratchpadPtr)
				hostScratchpadPtr = 0
			}
			totalHashesAttempted += workSize
			continue
		}

		clFinish.Call(ctx.commandQueue)

		// Kernel writes: results[0]=found(1/0), results[1]=nonce, results[2..33]=hash (low byte per uint)
		resultsData := make([]uint32, 34)
		ret, _, _ = clEnqueueReadBuffer.Call(
			ctx.commandQueue,
			resultsBuf,
			1, 0,
			uintptr(34*4),
			uintptr(unsafe.Pointer(&resultsData[0])),
			0, 0, 0,
		)
		if ret != CL_SUCCESS {
			log.Printf("⚠️ Failed to read results buffer: %d", ret)
			clReleaseMemObject.Call(headerBaseBuf)
			clReleaseMemObject.Call(targetBuf)
			clReleaseMemObject.Call(resultsBuf)
			runtime.KeepAlive(hostScratchpad)
			clReleaseMemObject.Call(scratchpadBuf)
			if ctx.useVirtualAllocScratch && hostScratchpadPtr != 0 {
				freePageAligned(hostScratchpadPtr)
				hostScratchpadPtr = 0
			}
			totalHashesAttempted += workSize
			continue
		}

		batchFound := resultsData[0] == 1
		var batchNonce uint32
		var batchHash []byte
		if batchFound {
			batchNonce = resultsData[1]
			batchHash = make([]byte, 32)
			for i := 0; i < 32; i++ {
				batchHash[i] = byte(resultsData[2+i] & 0xff)
			}
			log.Printf("🎉 GPU found block! Nonce: %d", batchNonce)
		}

		clReleaseMemObject.Call(headerBaseBuf)
		clReleaseMemObject.Call(targetBuf)
		clReleaseMemObject.Call(resultsBuf)
		runtime.KeepAlive(hostScratchpad)
		clReleaseMemObject.Call(scratchpadBuf)
		if ctx.useVirtualAllocScratch && hostScratchpadPtr != 0 {
			freePageAligned(hostScratchpadPtr)
			hostScratchpadPtr = 0
		}

		totalHashesAttempted += workSize

		if batchFound {
			return batchNonce, batchHash, true, totalHashesAttempted
		}
	}
	
	// No block found after all batches (reusable buffers released by defer if used)
	return 0, nil, false, totalHashesAttempted
}

// mineParallel uses multiple goroutines to mine in parallel with optimizations
func (g *GPUMiner) mineParallel(block *Block, targetBig *big.Int) (uint32, []byte, bool, uint64) {
	g.cancelMining.Store(false)
	var foundNonce uint32
	var foundHash []byte
	var found atomic.Bool
	var totalHashes atomic.Uint64

	// Progress logging: report every 5s; if no progress for 60s log "still mining"; after 90s cancel round (avoid hang)
	progressDone := make(chan struct{})
	defer close(progressDone)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		var lastN uint64
		var lastT time.Time
		var lastProgressTime time.Time
		var lastRateKH float64
		var roundCompleteLogged bool
		var lastStuckLog time.Time
		for {
			select {
			case <-progressDone:
				return
			case <-ticker.C:
				n := totalHashes.Load()
				now := time.Now()
				if n == 0 {
					continue
				}
				if lastN == 0 {
					lastN, lastT, lastProgressTime = n, now, now
					log.Printf("⛏️ Mining... %d hashes", n)
					continue
				}
				deltaN := n - lastN
				deltaSec := now.Sub(lastT).Seconds()
				if deltaN > 0 {
					lastProgressTime = now
				}
				lastN, lastT = n, now
				if deltaSec < 0.5 {
					continue
				}
				rateKH := float64(deltaN) / deltaSec / 1000
				if deltaN > 0 {
					lastRateKH = rateKH
					roundCompleteLogged = false
					log.Printf("⛏️ Mining... %d hashes (%.1f KH/s)", n, rateKH)
				} else {
					// No progress this tick
					stuckSec := now.Sub(lastProgressTime).Seconds()
					if stuckSec >= 90 {
						log.Printf("⛏️ No progress for 90s (stuck at %d hashes) — cancelling round to avoid hang", n)
						g.cancelMining.Store(true)
						continue
					}
					if stuckSec >= 60 {
						if now.Sub(lastStuckLog) >= 30*time.Second || lastStuckLog.IsZero() {
							log.Printf("⛏️ Still mining... %d hashes (no new progress for %.0fs — possible hang)", n, stuckSec)
							lastStuckLog = now
						}
						roundCompleteLogged = true
					} else if !roundCompleteLogged && lastRateKH > 0 {
						log.Printf("⛏️ Mining... %d hashes (round complete, was %.1f KH/s)", n, lastRateKH)
						roundCompleteLogged = true
					}
				}
			}
		}
	}()

	// Pre-compute static header parts (everything except nonce) for performance
	headerBase := g.precomputeHeaderBase(block)

	// Calculate hashes per thread
	hashesPerThread := g.effectiveMaxHashes() / uint64(g.ThreadCount)
	if hashesPerThread == 0 {
		hashesPerThread = 1000 // Minimum per thread
	}

	var wg sync.WaitGroup
	results := make(chan struct {
		nonce uint32
		hash  []byte
	}, g.ThreadCount)

	// Calculate starting nonce based on strategy
	startNonceBase := calculateStartNonce(block, g.NonceStartMode)
	
	// Determine nonce distribution strategy
	var nonceRangePerThread uint64
	if g.UseStride {
		nonceRangePerThread = 0 // Not used in stride mode
	} else {
		nonceRangePerThread = uint64(0xFFFFFFFF) / uint64(g.ThreadCount)
	}

	// Start parallel mining threads
	for t := 0; t < g.ThreadCount; t++ {
		wg.Add(1)
		go func(threadID int) {
			defer wg.Done()

			// Pin goroutine to CPU core for better cache locality
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			localHashCount := uint64(0)
			maxLocalHashes := hashesPerThread

			// Pre-allocate buffers
			headerBuf := make([]byte, len(headerBase)+4)
			copy(headerBuf, headerBase)
			
			// Pre-compute target bytes for fast comparison (reuse across all hashes)
			targetBytes := targetBig.Bytes()
			targetBytesLE := make([]byte, 32)
			copy(targetBytesLE[32-len(targetBytes):], targetBytes)
			reverseBytesInPlace(targetBytesLE)
			
			// Determine nonce iteration strategy
			var threadStartNonce, threadEndNonce uint32
			var stride uint32
			
			if g.UseStride {
				stride = uint32(g.ThreadCount)
				threadStartNonce = (startNonceBase + uint32(threadID)) % 0xFFFFFFFF
				threadEndNonce = 0xFFFFFFFF
			} else {
				threadStartNonce = startNonceBase + uint32(uint64(threadID)*nonceRangePerThread)
				threadEndNonce = startNonceBase + uint32(uint64(threadID+1)*nonceRangePerThread)
				if threadID == g.ThreadCount-1 {
					threadEndNonce = 0xFFFFFFFF
				}
			}

			currentNonce := threadStartNonce
			
			// Process nonces based on search mode
			if g.NonceSearchMode == "backward" {
				// Backward search
				for localHashCount < maxLocalHashes && currentNonce > 0 && !found.Load() {
					batchEnd := currentNonce
					if currentNonce > uint32(g.BatchSize) {
						batchEnd = currentNonce - uint32(g.BatchSize)
					} else {
						batchEnd = 0
					}
					
					for nonce := currentNonce; nonce > batchEnd && localHashCount < maxLocalHashes && !found.Load(); nonce-- {
						binary.LittleEndian.PutUint32(headerBuf[len(headerBase):], nonce)
						hash := g.scryptHashFast(headerBuf)
						localHashCount++
						if localHashCount%500 == 0 {
							if g.cancelMining.Load() {
								return
							}
							totalHashes.Add(500)
						}

						if hashMeetsTargetLE(hash, targetBytesLE) {
							if !found.Swap(true) {
								results <- struct {
									nonce uint32
									hash  []byte
								}{nonce, hash}
							}
							return
						}
					}
					
					currentNonce = batchEnd
					if currentNonce == 0 && localHashCount < maxLocalHashes {
						currentNonce = 0xFFFFFFFF
					}
				}
			} else {
				// Forward or sequential search
				for localHashCount < maxLocalHashes && !found.Load() {
					if g.UseStride {
						// Stride-based: nonce = startNonce + i*stride
						for i := uint32(0); i < uint32(g.BatchSize) && localHashCount < maxLocalHashes && !found.Load(); i++ {
							nonce := (threadStartNonce + i*stride) % 0xFFFFFFFF
							binary.LittleEndian.PutUint32(headerBuf[len(headerBase):], nonce)
							hash := g.scryptHashFast(headerBuf)
							localHashCount++
							if localHashCount%500 == 0 {
								if g.cancelMining.Load() {
									return
								}
								totalHashes.Add(500)
							}

							if hashMeetsTargetLE(hash, targetBytesLE) {
								if !found.Swap(true) {
									results <- struct {
										nonce uint32
										hash  []byte
									}{nonce, hash}
								}
								return
							}
						}
						threadStartNonce = (threadStartNonce + uint32(g.BatchSize)*stride) % 0xFFFFFFFF
						continue
					} else {
						// Sequential range-based
						batchEnd := currentNonce + uint32(g.BatchSize)
						if batchEnd > threadEndNonce {
							batchEnd = threadEndNonce
						}
						
						for nonce := currentNonce; nonce < batchEnd && localHashCount < maxLocalHashes && !found.Load(); nonce++ {
							binary.LittleEndian.PutUint32(headerBuf[len(headerBase):], nonce)
							hash := g.scryptHashFast(headerBuf)
							localHashCount++
							if localHashCount%500 == 0 {
								if g.cancelMining.Load() {
									return
								}
								totalHashes.Add(500)
							}

							if hashMeetsTargetLE(hash, targetBytesLE) {
								if !found.Swap(true) {
									results <- struct {
										nonce uint32
										hash  []byte
									}{nonce, hash}
								}
								return
							}
						}
						
						currentNonce = batchEnd
						
						if currentNonce >= threadEndNonce && localHashCount < maxLocalHashes {
							currentNonce = threadStartNonce
						}
					}
				}
			}

			// Add remainder (we already added 500 every 500 hashes during the loop)
			totalHashes.Add(localHashCount % 500)
		}(t)
	}

	// Wait for first result or all threads to complete
	done := make(chan bool, 1)
	go func() {
		wg.Wait()
		close(results)
		done <- true
	}()

	// Try to get a result
	select {
	case result, ok := <-results:
		if ok && result.nonce != 0 && len(result.hash) > 0 {
			foundNonce = result.nonce
			foundHash = result.hash
			found.Store(true)
			wg.Wait()
			// Double-check the hash meets the target (fast comparison)
			targetBytes := targetBig.Bytes()
			targetBytesLE := make([]byte, 32)
			copy(targetBytesLE[32-len(targetBytes):], targetBytes)
			reverseBytesInPlace(targetBytesLE)
			if hashMeetsTargetLE(foundHash, targetBytesLE) {
				return foundNonce, foundHash, true, totalHashes.Load()
			}
			log.Printf("⚠️ False positive detected in miner - hash doesn't meet target")
		}
		<-done
		return 0, nil, false, totalHashes.Load()
	case <-done:
		return 0, nil, false, totalHashes.Load()
	}
}

// precomputeHeaderBase pre-computes the static part of the block header (without nonce)
func (g *GPUMiner) precomputeHeaderBase(block *Block) []byte {
	blockCopy := *block
	blockCopy.Nonce = 0
	header := blockCopy.SerializeHeader()
	return header[:len(header)-4]
}

// scryptHashFast computes Dogecoin PoW: scrypt(header80, header80) -> 32 bytes (no SHA256 wrapper).
func (g *GPUMiner) scryptHashFast(header80 []byte) []byte {
	if len(header80) != 80 {
		panic(fmt.Sprintf("scryptHashFast requires 80-byte header, got %d", len(header80)))
	}
	hash, err := scrypt.Key(header80, header80, 1024, 1, 1, 32)
	if err != nil {
		panic(fmt.Sprintf("Scrypt error: %v", err))
	}
	return hash
}

// DetectGPUs returns information about available GPUs
func DetectGPUs() []string {
	ctx, err := InitializeOpenCL()
	if err != nil {
		cores := runtime.NumCPU()
		return []string{fmt.Sprintf("CPU: %d cores available", cores)}
	}
	defer ctx.CleanupOpenCL()
	
	var devices []string
	for i, device := range ctx.devices {
		var deviceName [128]byte
		var nameSize uint64
		clGetDeviceInfo.Call(device, CL_DEVICE_NAME, 128, uintptr(unsafe.Pointer(&deviceName[0])), uintptr(unsafe.Pointer(&nameSize)))
		if nameSize > 0 {
			name := string(deviceName[:nameSize-1])
			devices = append(devices, fmt.Sprintf("GPU %d: %s", i+1, name))
		}
	}
	return devices
}

// IsGPUAvailable checks if GPU mining is available
func IsGPUAvailable() bool {
	ctx, err := InitializeOpenCL()
	if err != nil {
		return false
	}
	defer ctx.CleanupOpenCL()
	return ctx.initialized && ctx.deviceType == "GPU"
}

// generateSmartNonces creates a mix of "lucky" nonces using multiple prediction strategies
// This improves the chance of finding a winning hash by trying strategic patterns first
// Note: We can't pre-generate hashes because the block header changes with each new block
// (timestamp, merkle root, previous block hash all change), so we generate smart nonces
// on-the-fly based on the current block's characteristics
// seedOffset: used to vary nonce generation across batches for better randomization
func (g *GPUMiner) generateSmartNonces(block *Block, count uint64, seedOffset uint64) []uint32 {
	nonces := make([]uint32, count)
	nonceSet := make(map[uint32]bool) // Track used nonces to avoid duplicates
	idx := uint64(0)
	
	// Strategy 1: "Lucky" pattern nonces (commonly found in real blocks)
	luckyPatterns := []uint32{
		0x00000000, 0xFFFFFFFF, 0x12345678, 0xABCDEF00,
		0x11111111, 0x22222222, 0x33333333, 0x44444444,
		0x55555555, 0x66666666, 0x77777777, 0x88888888,
		0x99999999, 0xAAAAAAAA, 0xBBBBBBBB, 0xCCCCCCCC,
		0xDDDDDDDD, 0xEEEEEEEE, 0xFEDCBA98, 0x76543210,
		0xDEADBEEF, 0xCAFEBABE, 0xBAADF00D, 0x13371337,
	}
	
	for _, pattern := range luckyPatterns {
		if idx >= count {
			break
		}
		if !nonceSet[pattern] {
			nonces[idx] = pattern
			nonceSet[pattern] = true
			idx++
		}
	}
	
	// Strategy 2: Block-height based nonces (sometimes patterns emerge)
	// Vary based on seedOffset for better randomization across batches
	blockHeightNonce := uint32(block.Version) ^ uint32(block.Timestamp) ^ uint32(block.Bits) ^ uint32(seedOffset&0xFFFFFFFF)
	for i := uint32(0); i < 100 && idx < count; i++ {
		nonce := blockHeightNonce + i*0x10001 // Use prime-like interval
		if !nonceSet[nonce] {
			nonces[idx] = nonce
			nonceSet[nonce] = true
			idx++
		}
	}
	
	// Strategy 3: Timestamp-based nonces (time can influence hash patterns)
	// Add seedOffset for variation across batches
	timestampNonce := uint32(block.Timestamp) ^ uint32((seedOffset>>32)&0xFFFFFFFF)
	for i := uint32(0); i < 200 && idx < count; i++ {
		nonce := timestampNonce ^ (i * 0x1337) // XOR with pattern
		if !nonceSet[nonce] {
			nonces[idx] = nonce
			nonceSet[nonce] = true
			idx++
		}
	}
	
	// Strategy 4: Merkle root derived nonces (block content influences)
	merkleNonce := uint32(block.MerkleRoot[0]) | (uint32(block.MerkleRoot[1]) << 8) |
		(uint32(block.MerkleRoot[2]) << 16) | (uint32(block.MerkleRoot[3]) << 24)
	for i := uint32(0); i < 300 && idx < count; i++ {
		nonce := merkleNonce + i*0x2710 // Add intervals
		if !nonceSet[nonce] {
			nonces[idx] = nonce
			nonceSet[nonce] = true
			idx++
		}
	}
	
	// Strategy 5: Prime number intervals (mathematical patterns)
	primes := []uint32{2, 3, 5, 7, 11, 13, 17, 19, 23, 29, 31, 37, 41, 43, 47, 53, 59, 61, 67, 71, 73, 79, 83, 89, 97, 101, 103, 107, 109, 113}
	baseNonce := uint32(block.Timestamp) & 0xFFFF0000 // Use upper bits
	for _, prime := range primes {
		if idx >= count {
			break
		}
		for mult := uint32(0); mult < 500 && idx < count; mult++ {
			nonce := (baseNonce + prime*mult) % 0xFFFFFFFF
			if !nonceSet[nonce] {
				nonces[idx] = nonce
				nonceSet[nonce] = true
				idx++
			}
		}
	}
	
	// Strategy 6: Bit-flip patterns (try nonces with specific bit characteristics)
	bitPatterns := []struct {
		mask  uint32
		value uint32
	}{
		{0xFF000000, 0x00000000}, // Leading zeros
		{0xFF000000, 0xFF000000}, // Leading ones
		{0x00FF0000, 0x0000FF00}, // Middle patterns
		{0x0000FFFF, 0x0000FFFF}, // Trailing patterns
		{0xFFFF0000, 0x0000FFFF}, // Alternating
	}
	
	for _, pattern := range bitPatterns {
		if idx >= count {
			break
		}
		for j := uint32(0); j < 1000 && idx < count; j++ {
			nonce := (pattern.value & pattern.mask) | (j &^ pattern.mask)
			if !nonceSet[nonce] {
				nonces[idx] = nonce
				nonceSet[nonce] = true
				idx++
			}
		}
	}
	
	// Strategy 7: Fibonacci-like sequences (natural patterns)
	fib1, fib2 := uint32(1), uint32(1)
	for i := uint32(0); i < 2000 && idx < count; i++ {
		nonce := (fib1 + fib2) % 0xFFFFFFFF
		if !nonceSet[nonce] {
			nonces[idx] = nonce
			nonceSet[nonce] = true
			idx++
		}
		fib1, fib2 = fib2, nonce
	}
	
	// Strategy 8: Fill remaining with optimized sequential (ensures full coverage)
	// Use a smart starting point based on block data and seedOffset for randomization
	smartStart := uint32(block.Timestamp^block.Bits) ^ uint32(seedOffset&0xFFFFFFFF)
	smartStart = smartStart % (0xFFFFFFFF - uint32(count-idx))
	if smartStart == 0 {
		smartStart = 1 // Avoid zero
	}
	for i := uint64(0); idx < count; i++ {
		nonce := smartStart + uint32(i) + uint32(seedOffset%1000000) // Add seed variation
		if nonce == 0 {
			nonce = 1 // Skip zero
		}
		if !nonceSet[nonce] {
			nonces[idx] = nonce
			nonceSet[nonce] = true
			idx++
		}
	}
	
	return nonces
}
