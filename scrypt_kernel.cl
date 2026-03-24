// Complete Dogecoin Scrypt OpenCL Kernel (N=1024, r=1, p=1)
// Full RFC 7914 compliant implementation for GPU mining

#define ROTL32(x, n) rotate(x, (uint)n)
#define ROTR32(x, n) rotate(x, (uint)(32-n))

// SHA-256 constants
__constant uint K[64] = {
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2
};

// SHA-256 Ch and Maj functions
#define Ch(x, y, z) ((x & y) ^ (~x & z))
#define Maj(x, y, z) ((x & y) ^ (x & z) ^ (y & z))
#define Sigma0(x) (ROTR32(x, 2) ^ ROTR32(x, 13) ^ ROTR32(x, 22))
#define Sigma1(x) (ROTR32(x, 6) ^ ROTR32(x, 11) ^ ROTR32(x, 25))
#define sigma0(x) (ROTR32(x, 7) ^ ROTR32(x, 18) ^ (x >> 3))
#define sigma1(x) (ROTR32(x, 17) ^ ROTR32(x, 19) ^ (x >> 10))

// SHA-256 transform
void sha256_transform(__private uint *state, __private const uchar *data) {
    uint W[64];
    uint a, b, c, d, e, f, g, h, T1, T2;
    
    // Prepare message schedule
    for (int i = 0; i < 16; i++) {
        W[i] = ((uint)data[i*4] << 24) | ((uint)data[i*4+1] << 16) |
               ((uint)data[i*4+2] << 8) | ((uint)data[i*4+3]);
    }
    for (int i = 16; i < 64; i++) {
        W[i] = sigma1(W[i-2]) + W[i-7] + sigma0(W[i-15]) + W[i-16];
    }
    
    // Initialize working variables
    a = state[0]; b = state[1]; c = state[2]; d = state[3];
    e = state[4]; f = state[5]; g = state[6]; h = state[7];
    
    // Main loop
    for (int i = 0; i < 64; i++) {
        T1 = h + Sigma1(e) + Ch(e, f, g) + K[i] + W[i];
        T2 = Sigma0(a) + Maj(a, b, c);
        h = g; g = f; f = e; e = d + T1;
        d = c; c = b; b = a; a = T1 + T2;
    }
    
    // Add to state
    state[0] += a; state[1] += b; state[2] += c; state[3] += d;
    state[4] += e; state[5] += f; state[6] += g; state[7] += h;
}

// SHA-256 hash of a message of exactly 'len' bytes (len <= 55 for single-block, else two blocks).
// Used to hash HMAC keys longer than 64 bytes per RFC 2104.
void sha256_hash80(__private const uchar *msg, __private uchar *digest) {
    uint state[8];
    uchar buf[64];
    state[0] = 0x6a09e667; state[1] = 0xbb67ae85;
    state[2] = 0x3c6ef372; state[3] = 0xa54ff53a;
    state[4] = 0x510e527f; state[5] = 0x9b05688c;
    state[6] = 0x1f83d9ab; state[7] = 0x5be0cd19;
    // First block: bytes 0..63
    for (int i = 0; i < 64; i++) buf[i] = msg[i];
    sha256_transform(state, buf);
    // Second block: bytes 64..79 + padding
    for (int i = 0; i < 64; i++) buf[i] = 0;
    for (int i = 0; i < 16; i++) buf[i] = msg[64 + i];
    buf[16] = 0x80;
    // Length = 80 bytes * 8 = 640 bits = 0x0280
    buf[62] = 0x02; buf[63] = 0x80;
    sha256_transform(state, buf);
    // Serialize
    for (int i = 0; i < 8; i++) {
        digest[i*4]   = (state[i] >> 24) & 0xFF;
        digest[i*4+1] = (state[i] >> 16) & 0xFF;
        digest[i*4+2] = (state[i] >>  8) & 0xFF;
        digest[i*4+3] =  state[i]        & 0xFF;
    }
}

// PBKDF2-HMAC-SHA256: one block (32 bytes) with given block_index (1..4 for Scrypt 128-byte B). Matches Dogecoin Core scrypt.cpp.
void pbkdf2_sha256_block(__private const uchar *password, uint pwlen,
                         __private const uchar *salt, uint saltlen,
                         uint block_index, __private uchar *output) {
    uchar ipad[64], opad[64];
    uchar buffer[64];
    uint istate[8], ostate[8], finalstate[8];
    
    // RFC 2104: if key length > block size (64), hash the key first
    if (pwlen > 64) {
        uchar hashed_key[32];
        sha256_hash80(password, hashed_key);
        for (int i = 0; i < 32; i++) {
            ipad[i] = hashed_key[i] ^ 0x36;
            opad[i] = hashed_key[i] ^ 0x5c;
        }
        for (int i = 32; i < 64; i++) {
            ipad[i] = 0x36;
            opad[i] = 0x5c;
        }
    } else {
        for (int i = 0; i < 64; i++) {
            uchar k = (i < pwlen) ? password[i] : 0;
            ipad[i] = k ^ 0x36;
            opad[i] = k ^ 0x5c;
        }
    }
    
    istate[0] = 0x6a09e667; istate[1] = 0xbb67ae85;
    istate[2] = 0x3c6ef372; istate[3] = 0xa54ff53a;
    istate[4] = 0x510e527f; istate[5] = 0x9b05688c;
    istate[6] = 0x1f83d9ab; istate[7] = 0x5be0cd19;
    sha256_transform(istate, ipad);
    
    ostate[0] = 0x6a09e667; ostate[1] = 0xbb67ae85;
    ostate[2] = 0x3c6ef372; ostate[3] = 0xa54ff53a;
    ostate[4] = 0x510e527f; ostate[5] = 0x9b05688c;
    ostate[6] = 0x1f83d9ab; ostate[7] = 0x5be0cd19;
    sha256_transform(ostate, opad);
    
    // Inner hash: H(K_ipad || salt || INT(block_index)) — salt 80 bytes, INT = 4 bytes BE (RFC 8018 / Dogecoin Core)
    for (int i = 0; i < 8; i++) finalstate[i] = istate[i];
    
    for (int i = 0; i < 64; i++) buffer[i] = (i < saltlen) ? salt[i] : 0;
    sha256_transform(finalstate, buffer);
    
    for (int i = 0; i < 64; i++) buffer[i] = 0;
    for (int i = 0; i < 16 && (64+i) < saltlen; i++) buffer[i] = salt[64 + i];
    buffer[16] = (block_index >> 24) & 0xFF;
    buffer[17] = (block_index >> 16) & 0xFF;
    buffer[18] = (block_index >> 8) & 0xFF;
    buffer[19] = block_index & 0xFF;
    buffer[20] = 0x80;
    buffer[56] = 0; buffer[57] = 0; buffer[58] = 0; buffer[59] = 0;
    // Length = (64 + 80 + 4) bytes * 8 bits = 1184 bits = 0x04A0
    buffer[60] = 0; buffer[61] = 0; buffer[62] = 0x04; buffer[63] = 0xA0;
    sha256_transform(finalstate, buffer);
    
    // Outer HMAC: serialize inner hash (in finalstate) into buffer BEFORE overwriting finalstate with ostate.
    // Previously the assignment finalstate=ostate came first, corrupting the inner hash bytes written to buffer.
    for (int i = 0; i < 64; i++) buffer[i] = 0;
    // Write inner hash result
    for (int i = 0; i < 8; i++) {
        buffer[i*4]   = (finalstate[i] >> 24) & 0xFF;
        buffer[i*4+1] = (finalstate[i] >> 16) & 0xFF;
        buffer[i*4+2] = (finalstate[i] >> 8) & 0xFF;
        buffer[i*4+3] = finalstate[i] & 0xFF;
    }
    for (int i = 0; i < 8; i++) finalstate[i] = ostate[i];
    buffer[32] = 0x80;
    buffer[62] = 3; buffer[63] = 0; // length = 768 bits
    sha256_transform(finalstate, buffer);
    
    // Write output
    for (int i = 0; i < 8; i++) {
        output[i*4]   = (finalstate[i] >> 24) & 0xFF;
        output[i*4+1] = (finalstate[i] >> 16) & 0xFF;
        output[i*4+2] = (finalstate[i] >> 8) & 0xFF;
        output[i*4+3] = finalstate[i] & 0xFF;
    }
}

// Salsa20/8 core
void salsa20_8(__private uint *B) {
    uint x[16];
    for (int i = 0; i < 16; i++) x[i] = B[i];
    
    for (int i = 0; i < 4; i++) { // 4 double rounds = 8 rounds
        // Column round
        x[ 4] ^= ROTL32(x[ 0] + x[12],  7);  x[ 8] ^= ROTL32(x[ 4] + x[ 0],  9);
        x[12] ^= ROTL32(x[ 8] + x[ 4], 13);  x[ 0] ^= ROTL32(x[12] + x[ 8], 18);
        x[ 9] ^= ROTL32(x[ 5] + x[ 1],  7);  x[13] ^= ROTL32(x[ 9] + x[ 5],  9);
        x[ 1] ^= ROTL32(x[13] + x[ 9], 13);  x[ 5] ^= ROTL32(x[ 1] + x[13], 18);
        x[14] ^= ROTL32(x[10] + x[ 6],  7);  x[ 2] ^= ROTL32(x[14] + x[10],  9);
        x[ 6] ^= ROTL32(x[ 2] + x[14], 13);  x[10] ^= ROTL32(x[ 6] + x[ 2], 18);
        x[ 3] ^= ROTL32(x[15] + x[11],  7);  x[ 7] ^= ROTL32(x[ 3] + x[15],  9);
        x[11] ^= ROTL32(x[ 7] + x[ 3], 13);  x[15] ^= ROTL32(x[11] + x[ 7], 18);
        // Row round
        x[ 1] ^= ROTL32(x[ 0] + x[ 3],  7);  x[ 2] ^= ROTL32(x[ 1] + x[ 0],  9);
        x[ 3] ^= ROTL32(x[ 2] + x[ 1], 13);  x[ 0] ^= ROTL32(x[ 3] + x[ 2], 18);
        x[ 6] ^= ROTL32(x[ 5] + x[ 4],  7);  x[ 7] ^= ROTL32(x[ 6] + x[ 5],  9);
        x[ 4] ^= ROTL32(x[ 7] + x[ 6], 13);  x[ 5] ^= ROTL32(x[ 4] + x[ 7], 18);
        x[11] ^= ROTL32(x[10] + x[ 9],  7);  x[ 8] ^= ROTL32(x[11] + x[10],  9);
        x[ 9] ^= ROTL32(x[ 8] + x[11], 13);  x[10] ^= ROTL32(x[ 9] + x[ 8], 18);
        x[12] ^= ROTL32(x[15] + x[14],  7);  x[13] ^= ROTL32(x[12] + x[15],  9);
        x[14] ^= ROTL32(x[13] + x[12], 13);  x[15] ^= ROTL32(x[14] + x[13], 18);
    }
    
    for (int i = 0; i < 16; i++) B[i] += x[i];
}

// BlockMix
void blockmix_salsa8(__private uint *B, __private uint *Y) {
    uint X[16];
    
    // X = B[2r-1] = B[1] (since r=1)
    for (int i = 0; i < 16; i++) X[i] = B[16 + i];
    
    // For i = 0 to 2r-1 (i.e., 0 and 1)
    for (int i = 0; i < 2; i++) {
        for (int j = 0; j < 16; j++) X[j] ^= B[i*16 + j];
        salsa20_8(X);
        for (int j = 0; j < 16; j++) Y[i*16 + j] = X[j];
    }
    
    // B = (Y[0], Y[1])
    for (int i = 0; i < 32; i++) B[i] = Y[i];
}

// ROMix for N=1024
void romix(__private uint *B, __global uint *V) {
    uint Y[32];
    int N = 1024;
    
    // Step 1: X = B, for i = 0 to N-1: V[i] = X; X = BlockMix(X)
    for (int i = 0; i < N; i++) {
        for (int j = 0; j < 32; j++) V[i*32 + j] = B[j];
        blockmix_salsa8(B, Y);
    }
    
    // Step 2: for i = 0 to N-1: j = Integerify(X) mod N; X = BlockMix(X xor V[j])
    for (int i = 0; i < N; i++) {
        uint j = B[16] & (N - 1); // Integerify = B[16]
        for (int k = 0; k < 32; k++) B[k] ^= V[j*32 + k];
        blockmix_salsa8(B, Y);
    }
}

// ROMix step 1 only: fill V (write-only). Used by split-kernel path when driver rejects READ_WRITE.
void romix_fill(__private uint *B, __global uint *V) {
    uint Y[32];
    int N = 1024;
    for (int i = 0; i < N; i++) {
        for (int j = 0; j < 32; j++) V[i*32 + j] = B[j];
        blockmix_salsa8(B, Y);
    }
}

// ROMix step 2 only: read from V (read-only). Used by split-kernel path.
void romix_mix(__private uint *B, __global const uint *V) {
    uint Y[32];
    int N = 1024;
    for (int i = 0; i < N; i++) {
        uint j = B[16] & (N - 1);
        for (int k = 0; k < 32; k++) B[k] ^= V[j*32 + k];
        blockmix_salsa8(B, Y);
    }
}

// PBKDF2 with 128-byte salt, block 1 only (for final Scrypt step). Message = salt(128) || INT(1) = 132 bytes.
void pbkdf2_sha256_salt128_block1(__private const uchar *password, uint pwlen,
                                  __private const uchar *salt128,
                                  __private uchar *output) {
    uchar ipad[64], opad[64];
    uchar buffer[64];
    uint istate[8], ostate[8], finalstate[8];
    
    // RFC 2104: if key length > block size (64), hash the key first
    if (pwlen > 64) {
        uchar hashed_key[32];
        sha256_hash80(password, hashed_key);
        for (int i = 0; i < 32; i++) {
            ipad[i] = hashed_key[i] ^ 0x36;
            opad[i] = hashed_key[i] ^ 0x5c;
        }
        for (int i = 32; i < 64; i++) {
            ipad[i] = 0x36;
            opad[i] = 0x5c;
        }
    } else {
        for (int i = 0; i < 64; i++) {
            uchar k = (i < pwlen) ? password[i] : 0;
            ipad[i] = k ^ 0x36;
            opad[i] = k ^ 0x5c;
        }
    }
    istate[0] = 0x6a09e667; istate[1] = 0xbb67ae85;
    istate[2] = 0x3c6ef372; istate[3] = 0xa54ff53a;
    istate[4] = 0x510e527f; istate[5] = 0x9b05688c;
    istate[6] = 0x1f83d9ab; istate[7] = 0x5be0cd19;
    sha256_transform(istate, ipad);
    ostate[0] = 0x6a09e667; ostate[1] = 0xbb67ae85;
    ostate[2] = 0x3c6ef372; ostate[3] = 0xa54ff53a;
    ostate[4] = 0x510e527f; ostate[5] = 0x9b05688c;
    ostate[6] = 0x1f83d9ab; ostate[7] = 0x5be0cd19;
    sha256_transform(ostate, opad);
    
    for (int i = 0; i < 8; i++) finalstate[i] = istate[i];
    for (int i = 0; i < 64; i++) buffer[i] = salt128[i];
    sha256_transform(finalstate, buffer);
    for (int i = 0; i < 64; i++) buffer[i] = salt128[64 + i];
    sha256_transform(finalstate, buffer);
    for (int i = 0; i < 64; i++) buffer[i] = 0;
    buffer[0] = 0; buffer[1] = 0; buffer[2] = 0; buffer[3] = 1;
    buffer[4] = 0x80;
    // Length = (64 ipad + 128 salt + 4 INT) * 8 = 1568 bits = 0x0620.
    // Must be at bytes 56-63 (big-endian 64-bit). Previous code put 0x0420 at bytes 58-59, yielding garbage.
    buffer[56] = 0; buffer[57] = 0; buffer[58] = 0; buffer[59] = 0;
    buffer[60] = 0; buffer[61] = 0; buffer[62] = 0x06; buffer[63] = 0x20;
    sha256_transform(finalstate, buffer);
    // finalstate now holds inner digest; serialize to buffer for outer hash
    for (int i = 0; i < 8; i++) {
        buffer[i*4]   = (finalstate[i] >> 24) & 0xFF;
        buffer[i*4+1] = (finalstate[i] >> 16) & 0xFF;
        buffer[i*4+2] = (finalstate[i] >> 8) & 0xFF;
        buffer[i*4+3] = finalstate[i] & 0xFF;
    }
    for (int i = 0; i < 8; i++) finalstate[i] = ostate[i];
    buffer[32] = 0x80;
    for (int i = 33; i < 56; i++) buffer[i] = 0;
    buffer[56] = 0; buffer[57] = 0; buffer[58] = 0; buffer[59] = 0;
    buffer[60] = 0; buffer[61] = 0; buffer[62] = 0x03; buffer[63] = 0x00; // 768 bits
    sha256_transform(finalstate, buffer);
    for (int i = 0; i < 8; i++) {
        output[i*4]   = (finalstate[i] >> 24) & 0xFF;
        output[i*4+1] = (finalstate[i] >> 16) & 0xFF;
        output[i*4+2] = (finalstate[i] >> 8) & 0xFF;
        output[i*4+3] = finalstate[i] & 0xFF;
    }
}

// Complete scrypt hash
void scrypt_1024_1_1(__private const uchar *input, __private uchar *output, __global uint *V) {
    uchar B[128];
    uint X[32];
    
    // PBKDF2(password=input, salt=input, c=1, dkLen=128) — blocks 1..4 (Dogecoin Core)
    pbkdf2_sha256_block(input, 80, input, 80, 1, B);
    pbkdf2_sha256_block(input, 80, input, 80, 2, B + 32);
    pbkdf2_sha256_block(input, 80, input, 80, 3, B + 64);
    pbkdf2_sha256_block(input, 80, input, 80, 4, B + 96);
    
    // B from PBKDF2 is big-endian (HMAC-SHA256); convert to X as little-endian 32-bit words (matches Dogecoin Core le32dec(&B[4*k]))
    for (int i = 0; i < 32; i++) {
        X[i] = ((uint)B[i*4]) | ((uint)B[i*4+1] << 8) |
               ((uint)B[i*4+2] << 16) | ((uint)B[i*4+3] << 24);
    }
    
    // ROMix
    romix(X, V);
    
    // Convert back to bytes (little-endian per word, matches Dogecoin Core le32enc)
    for (int i = 0; i < 32; i++) {
        B[i*4]   = X[i] & 0xFF;
        B[i*4+1] = (X[i] >> 8) & 0xFF;
        B[i*4+2] = (X[i] >> 16) & 0xFF;
        B[i*4+3] = (X[i] >> 24) & 0xFF;
    }
    
    // Final PBKDF2(password=input, salt=B, c=1, dkLen=32)
    pbkdf2_sha256_salt128_block1(input, 80, B, output);
}

// Main mining kernel
__kernel void mine_scrypt(
    __global const uchar *headerBase,  // 76 bytes (header without nonce)
    const uint nonceStart,              // Starting nonce
    __global const uchar *target,       // 32 bytes target
    __global uint *results,             // Output: nonce if found
    __global uint *V                    // Scratchpad: 1024*32 uints per work item
) {
    int gid = get_global_id(0);
    __private uchar header[80];
    __private uchar hash[32];
    
    // Build header with nonce
    for (int i = 0; i < 76; i++) header[i] = headerBase[i];
    uint nonce = nonceStart + gid;
    header[76] = nonce & 0xFF;
    header[77] = (nonce >> 8) & 0xFF;
    header[78] = (nonce >> 16) & 0xFF;
    header[79] = (nonce >> 24) & 0xFF;
    
    // Compute scrypt hash
    scrypt_1024_1_1(header, hash, V + gid * 1024 * 32);
    
    // Compare hash to target: hash is big-endian (PBKDF2), target is little-endian (from host).
    // As 256-bit LE: hash_le[i] = hash[31-i]. Compare MSB first (i=31) down to LSB (i=0).
    bool found = true;
    for (int i = 31; i >= 0; i--) {
        uchar h = hash[31 - i];
        if (h < target[i]) break;
        if (h > target[i]) {
            found = false;
            break;
        }
    }
    
    // Store result if found
    if (found) {
        results[0] = 1;           // Found flag
        results[1] = nonce;        // Winning nonce
        for (int i = 0; i < 32; i++) {
            results[2 + i] = hash[i]; // Hash
        }
    }
}

// Split-kernel path (no READ_WRITE buffer): fill scratchpad then mix. Use when driver rejects CL_MEM_READ_WRITE.
__kernel void scrypt_fill(
    __global const uchar *headerBase,
    const uint nonceStart,
    __global uint *V_write,      // WRITE_ONLY scratchpad: 1024*32 uints per work item
    __global uint *stateOut     // WRITE_ONLY: 32 uints per work item (state after fill)
) {
    int gid = get_global_id(0);
    uchar header[80];
    uchar B[128];
    uint X[32];
    for (int i = 0; i < 76; i++) header[i] = headerBase[i];
    uint nonce = nonceStart + gid;
    header[76] = nonce & 0xFF;
    header[77] = (nonce >> 8) & 0xFF;
    header[78] = (nonce >> 16) & 0xFF;
    header[79] = (nonce >> 24) & 0xFF;
    pbkdf2_sha256_block(header, 80, header, 80, 1, B);
    pbkdf2_sha256_block(header, 80, header, 80, 2, B + 32);
    pbkdf2_sha256_block(header, 80, header, 80, 3, B + 64);
    pbkdf2_sha256_block(header, 80, header, 80, 4, B + 96);
    for (int i = 0; i < 32; i++) {
        X[i] = ((uint)B[i*4]) | ((uint)B[i*4+1] << 8) | ((uint)B[i*4+2] << 16) | ((uint)B[i*4+3] << 24);
    }
    romix_fill(X, V_write + gid * 1024 * 32);
    for (int i = 0; i < 32; i++) stateOut[gid * 32 + i] = X[i];
}

__kernel void scrypt_mix(
    __global const uchar *headerBase,
    const uint nonceStart,
    __global const uint *stateIn,  // READ_ONLY: 32 uints per work item
    __global const uint *V_read,   // READ_ONLY scratchpad
    __global const uchar *target,
    __global uint *results
) {
    int gid = get_global_id(0);
    uchar header[80];
    uchar B[128];
    uint X[32];
    uchar hash[32];
    for (int i = 0; i < 76; i++) header[i] = headerBase[i];
    uint nonce = nonceStart + gid;
    header[76] = nonce & 0xFF;
    header[77] = (nonce >> 8) & 0xFF;
    header[78] = (nonce >> 16) & 0xFF;
    header[79] = (nonce >> 24) & 0xFF;
    for (int i = 0; i < 32; i++) X[i] = stateIn[gid * 32 + i];
    romix_mix(X, V_read + gid * 1024 * 32);
    for (int i = 0; i < 32; i++) {
        B[i*4]   = X[i] & 0xFF;
        B[i*4+1] = (X[i] >> 8) & 0xFF;
        B[i*4+2] = (X[i] >> 16) & 0xFF;
        B[i*4+3] = (X[i] >> 24) & 0xFF;
    }
    pbkdf2_sha256_salt128_block1(header, 80, B, hash);
    // hash is BE, target is LE; compare as 256-bit LE: hash_le[i] = hash[31-i]
    bool found = true;
    for (int i = 31; i >= 0; i--) {
        uchar h = hash[31 - i];
        if (h < target[i]) break;
        if (h > target[i]) { found = false; break; }
    }
    if (found) {
        results[0] = 1;
        results[1] = nonce;
        for (int i = 0; i < 32; i++) results[2 + i] = hash[i];
    }
}
