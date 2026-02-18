package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"
)

// Dogecoin mainnet P2P network parameters.
// See dogecoin/dogecoin src/chainparams.cpp (pchMessageStart, nDefaultPort).
var (
	dogeMainnetMagic = [4]byte{0xc0, 0xc0, 0xc0, 0xc0}
	dogeMainnetPort  = 22556
	dogeDNSSeeds     = []string{
		"seed.multidoge.org",
		"seed2.multidoge.org",
	}
)

// Dogecoin testnet P2P parameters (chainparams.cpp CTestNetParams). DNS seeds only (vSeeds in Core).
var (
	dogeTestnetMagic = [4]byte{0xfc, 0xc1, 0xb7, 0xdc}
	dogeTestnetPort  = 44556
	dogeTestnetSeeds = []string{
		"testseed.jrn.me.uk", // only testnet DNS seed in chainparams.cpp
	}
)

// P2PClient is a very small Dogecoin P2P client used for:
// - discovering chain tip via headers
// - broadcasting found blocks
//
// It is NOT a full node: it tracks only headers and mines empty blocks.
type P2PClient struct {
	network            string   // "mainnet" or "testnet"
	seeds              []string
	port               int
	magic              [4]byte
	genesisHashWire    [32]byte // genesis block hash in wire order (for this network)
	checkpoints        map[string]checkpointEntry
	conn               net.Conn // primary connection for headers/sync
	peerAddr           string   // connected peer address for logging
	knownPeers         []string // peers discovered from seeds (host:port), try these before seeds
	badPeers           map[string]time.Time // peers that failed recently (addr -> failTime), don't retry for 5 minutes
	peersMux           sync.Mutex
	// Broadcast pool: additional connections for block broadcasting
	broadcastConns     map[string]net.Conn // addr -> conn
	broadcastMux       sync.RWMutex
	maxBroadcastPeers  int // max number of broadcast connections to maintain
	bestHeader             *BlockHeader
	bestHeight             int64
	lastHeadersReq         time.Time
	headerRefreshInterval  time.Duration // how often to re-fetch tip from network (shorter on testnet)
	startLocatorHash       [32]byte      // start from this block hash (wire order)
	startHeight        int64    // height of that block
	hasStartCheckpoint bool     // true if user set a checkpoint or manual hash
	logFunc            func(msg string)
	optionalPeerOverride string // if set, try this peer first (e.g. custom testnet node when seed is down)
	// Last version message from connected peer (for UI)
	peerVersion  int32
	peerServices uint64
	peerUserAgent string
}

// PeerInfo holds the version message fields from the connected P2P peer (for web UI).
type PeerInfo struct {
	Version   int32  `json:"version"`
	Services  uint64 `json:"services"`
	UserAgent string `json:"userAgent"`
}

// peerCacheFile returns the filename for persisting good peers for the current network.
func (p *P2PClient) peerCacheFile() string {
	if p.network == "testnet" {
		return "peers_testnet.json"
	}
	return "peers_mainnet.json"
}

// peerCacheData represents the JSON structure for peer cache
type peerCacheData struct {
	Network string   `json:"network"`
	Peers   []string `json:"peers"`
	Updated string   `json:"updated"` // timestamp
}

// isBadPeer checks if a peer recently failed and shouldn't be retried yet.
// Cleans up expired entries (peers are bad for 5 minutes after failure).
func (p *P2PClient) isBadPeer(addr string) bool {
	p.peersMux.Lock()
	defer p.peersMux.Unlock()
	if p.badPeers == nil {
		p.badPeers = make(map[string]time.Time)
		return false
	}
	failTime, exists := p.badPeers[addr]
	if !exists {
		return false
	}
	// Peers are bad for 2 minutes, then we can retry them (reduced from 5 for faster recovery)
	if time.Since(failTime) > 2*time.Minute {
		delete(p.badPeers, addr)
		return false
	}
	return true
}

// markBadPeer adds a peer to the bad list after connection failure.
func (p *P2PClient) markBadPeer(addr string) {
	p.peersMux.Lock()
	defer p.peersMux.Unlock()
	if p.badPeers == nil {
		p.badPeers = make(map[string]time.Time)
	}
	p.badPeers[addr] = time.Now()
	log.Printf("P2P: marked %s as bad peer (will retry after 2 minutes)", addr)
}

// filterBadPeers removes bad peers from a list and returns the cleaned list.
func (p *P2PClient) filterBadPeers(peers []string) []string {
	filtered := make([]string, 0, len(peers))
	for _, addr := range peers {
		if !p.isBadPeer(addr) {
			filtered = append(filtered, addr)
		}
	}
	return filtered
}

// loadCachedPeers reads previously-good peers from disk and prepends them to knownPeers.
func (p *P2PClient) loadCachedPeers() {
	data, err := os.ReadFile(p.peerCacheFile())
	if err != nil {
		return // no file yet, that's fine
	}
	
	var cache peerCacheData
	if err := json.Unmarshal(data, &cache); err != nil {
		// If JSON parsing fails, try legacy txt format for backward compatibility
		p.loadCachedPeersLegacy()
		return
	}
	
	loaded := cache.Peers
	if len(loaded) > 0 {
		// Filter out recently-failed peers
		loaded = p.filterBadPeers(loaded)
		if len(loaded) == 0 {
			return
		}
		rand.Shuffle(len(loaded), func(i, j int) { loaded[i], loaded[j] = loaded[j], loaded[i] })
		if len(loaded) > 50 {
			loaded = loaded[:50]
		}
		p.peersMux.Lock()
		p.knownPeers = append(loaded, p.knownPeers...)
		p.peersMux.Unlock()
		log.Printf("P2P: loaded %d cached peer(s) from %s", len(loaded), p.peerCacheFile())
	}
}

// loadCachedPeersLegacy loads from old txt format for backward compatibility
func (p *P2PClient) loadCachedPeersLegacy() {
	// Try old .txt filename
	txtFilename := strings.Replace(p.peerCacheFile(), ".json", ".txt", 1)
	f, err := os.Open(txtFilename)
	if err != nil {
		return
	}
	defer f.Close()
	
	var loaded []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && strings.Contains(line, ":") {
			loaded = append(loaded, line)
		}
	}
	if len(loaded) > 0 {
		loaded = p.filterBadPeers(loaded)
		if len(loaded) == 0 {
			return
		}
		rand.Shuffle(len(loaded), func(i, j int) { loaded[i], loaded[j] = loaded[j], loaded[i] })
		if len(loaded) > 50 {
			loaded = loaded[:50]
		}
		p.peersMux.Lock()
		p.knownPeers = append(loaded, p.knownPeers...)
		p.peersMux.Unlock()
		log.Printf("P2P: loaded %d cached peer(s) from legacy %s", len(loaded), txtFilename)
		
		// Migrate to JSON format
		p.savePeerCache(loaded)
		// Delete old txt file
		os.Remove(txtFilename)
	}
}

// savePeerToCache appends a successfully-connected peer address to the cache file (deduplicating).
func (p *P2PClient) savePeerToCache(addr string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return
	}
	filename := p.peerCacheFile()

	// Read existing peers
	var cache peerCacheData
	if data, err := os.ReadFile(filename); err == nil {
		json.Unmarshal(data, &cache)
	}
	
	// Check if already exists
	for _, peer := range cache.Peers {
		if peer == addr {
			return // already saved
		}
	}

	// Cap at 200 peers
	if len(cache.Peers) >= 200 {
		return
	}

	// Add new peer
	cache.Network = p.network
	cache.Peers = append(cache.Peers, addr)
	cache.Updated = time.Now().Format(time.RFC3339)
	
	// Write back to file
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(filename, data, 0644)
}

// savePeerCache saves a list of peers to the cache file (used for migration)
func (p *P2PClient) savePeerCache(peers []string) {
	cache := peerCacheData{
		Network: p.network,
		Peers:   peers,
		Updated: time.Now().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(p.peerCacheFile(), data, 0644)
}

// Dogecoin mainnet genesis block hash in wire/internal order (prevBlock in block 1 references this).
// Display order from chainparams: 1a91e3dace36e2be3bf030a65679fe821aa1d6ef92e7c9902eb318182c355691.
var dogeGenesisHashWire [32]byte

// Dogecoin testnet genesis (display): bb0a78264637406b6360aad926284d544d7049f45189db5664f3c4d07350559e
var dogeTestnetGenesisWire [32]byte

func init() {
	// Mainnet genesis: store in wire order (already reversed from display)
	genesisWire, _ := hex.DecodeString("9156352c1818b32e90c9e792efd6a11a82fe7956a630f03bbee236cedae3911a")
	copy(dogeGenesisHashWire[:], genesisWire)
	
	// Testnet genesis: store in wire order (already reversed from display)
	testnetWire, _ := hex.DecodeString("9e555073d0c4f36456db8951f449704d544d2826d9aa60636b40374626780abb")
	copy(dogeTestnetGenesisWire[:], testnetWire)
}

// Checkpoint entry for a network (height + hash in wire order).
type checkpointEntry struct {
	Height int64
	Hash   string
}

// Dogecoin mainnet checkpoints (height -> block hash in hex). Stored in WIRE order.
var dogeMainnetCheckpoints = map[string]checkpointEntry{
	"0":       {0, "9156352c1818b32e90c9e792efd6a11a82fe7956a630f03bbee236cedae3911a"},
	"3043797": {3043797, "a8fecc11d4f4e0b55aaf984095380a48a8414fa22041a94be5360bac2f877bdf6"},
	"5526282": {5526282, "42309cb45a5d4d5d1fe14894732a8e6da2071269fc8611bc2d01e0edad824205"},
	"6024440": {6024440, "8b487d1378c7662b1da34f849e91989837b50c2ee26fc5b154512b7ccf1e6a9c"},
}

// Dogecoin testnet checkpoints (genesis only - safe for testnet).
// Hash in DISPLAY order (what you see on block explorers) - code will reverse to wire order.
var dogeTestnetCheckpoints = map[string]checkpointEntry{
	"0":        {0, "bb0a78264637406b6360aad926284d544d7049f45189db5664f3c4d07350559e"},        // Genesis (display order from explorer)
	"38150909": {38150909, "c428bfe503bea0fccf67a48518346359dd7bc476d52731448c65bd61b129f02f"}, // Block 38150909 (display order from SoChain)
	// Note: Testnet has low hashrate and frequent reorgs, so non-genesis checkpoints may become orphaned
	// Checkpoint hashes are in DISPLAY order (as shown on block explorers); hexTo32Bytes will convert to wire order
}

func NewP2PClient(network string) *P2PClient {
	p := &P2PClient{
		broadcastConns:    make(map[string]net.Conn),
		maxBroadcastPeers: 8, // Maintain connections to 8 peers for block broadcasting
	}
	p.setNetwork(network)
	return p
}

// setNetwork sets magic, port, seeds, genesis, checkpoints, and tip-refresh interval for the given network.
// Testnet uses a shorter refresh (2s) so we don't mine on stale tips when blocks are every ~1s.
func (p *P2PClient) setNetwork(network string) {
	p.network = network
	if network == "testnet" {
		p.magic = dogeTestnetMagic
		p.port = dogeTestnetPort
		p.seeds = dogeTestnetSeeds
		p.genesisHashWire = dogeTestnetGenesisWire
		p.checkpoints = dogeTestnetCheckpoints
		p.headerRefreshInterval = 500 * time.Millisecond // fast blocks: stay on current tip
	} else {
		p.magic = dogeMainnetMagic
		p.port = dogeMainnetPort
		p.seeds = dogeDNSSeeds
		p.genesisHashWire = dogeGenesisHashWire
		p.checkpoints = dogeMainnetCheckpoints
		p.headerRefreshInterval = 15 * time.Second
	}
}

// SetNetwork switches to mainnet or testnet. Disconnects and clears known peers so next connect uses the new network.
func (p *P2PClient) SetNetwork(network string) {
	p.disconnect()
	p.peersMux.Lock()
	p.knownPeers = nil
	p.peersMux.Unlock()
	// Close all broadcast connections
	p.broadcastMux.Lock()
	for addr, conn := range p.broadcastConns {
		if conn != nil {
			conn.Close()
		}
		delete(p.broadcastConns, addr)
	}
	p.broadcastMux.Unlock()
	p.setNetwork(network)
}

// SetLogFunc sets a callback for P2P connection and tip updates (e.g. to broadcast to web UI).
func (p *P2PClient) SetLogFunc(f func(msg string)) {
	p.logFunc = f
}

// SetPeerOverride sets an optional peer address (e.g. "1.2.3.4:44556") to try first. Use when the DNS seed keeps disconnecting (e.g. testnet).
func (p *P2PClient) SetPeerOverride(addr string) {
	p.peersMux.Lock()
	defer p.peersMux.Unlock()
	p.optionalPeerOverride = strings.TrimSpace(addr)
}

// GetPeerAddr returns the address of the currently connected P2P peer (e.g. "seed.multidoge.org:22556"), or "" if disconnected.
func (p *P2PClient) GetPeerAddr() string {
	return p.peerAddr
}

// GetPeerInfo returns version message fields from the connected peer (zero values if disconnected).
func (p *P2PClient) GetPeerInfo() PeerInfo {
	return PeerInfo{Version: p.peerVersion, Services: p.peerServices, UserAgent: p.peerUserAgent}
}

// GetBroadcastPeers returns list of broadcast peer addresses
func (p *P2PClient) GetBroadcastPeers() []string {
	p.broadcastMux.RLock()
	defer p.broadcastMux.RUnlock()
	
	peers := make([]string, 0, len(p.broadcastConns))
	for addr := range p.broadcastConns {
		peers = append(peers, addr)
	}
	return peers
}

// SetStartCheckpoint sets the block to start header sync from (skip syncing from genesis).
// hashHex: block hash in hex (64 chars); height: that block's height.
// If hashHex is empty, clears the start checkpoint and the in-memory tip so the next
// getheaders uses genesis and we don't fail linkage after a reorg (old tip no longer on chain).
func (p *P2PClient) SetStartCheckpoint(hashHex string, height int64) error {
	if hashHex == "" {
		p.hasStartCheckpoint = false
		p.startLocatorHash = [32]byte{}
		p.startHeight = 0
		p.bestHeader = nil
		p.bestHeight = 0
		return nil
	}
	h, err := hexTo32Bytes(hashHex)
	if err != nil {
		return fmt.Errorf("invalid checkpoint hash: %v", err)
	}
	p.startLocatorHash = h
	p.startHeight = height
	p.hasStartCheckpoint = true
	return nil
}

// SetStartCheckpointByID sets start from a predefined checkpoint (e.g. "0", "6024440").
func (p *P2PClient) SetStartCheckpointByID(id string) error {
	cp, ok := p.checkpoints[id]
	if !ok {
		return fmt.Errorf("unknown checkpoint: %s", id)
	}
	return p.SetStartCheckpoint(cp.Hash, cp.Height)
}

// dialPreferredIPv4 connects to host:port, preferring IPv4 (avoids "forcibly closed" on some seeds over IPv6).
func dialPreferredIPv4(host, port string, timeout time.Duration) (net.Conn, string, error) {
	addr := net.JoinHostPort(host, port)
	ips, err := net.LookupIP(host)
	if err != nil {
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return nil, "", err
		}
		return conn, addr, nil
	}
	for _, ip := range ips {
		if ip.To4() != nil {
			c, err := net.DialTimeout("tcp4", net.JoinHostPort(ip.String(), port), timeout)
			if err != nil {
				continue
			}
			return c, net.JoinHostPort(ip.String(), port), nil
		}
	}
	for _, ip := range ips {
		if ip.To4() == nil {
			c, err := net.DialTimeout("tcp6", net.JoinHostPort(ip.String(), port), timeout)
			if err != nil {
				continue
			}
			return c, net.JoinHostPort(ip.String(), port), nil
		}
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, "", err
	}
	return conn, addr, nil
}

// tryConnect dials addr and performs version/verack handshake. On success sets p.conn and p.peerAddr.
func (p *P2PClient) tryConnect(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		log.Printf("P2P: connecting to %s (tcp, 15s timeout)...", addr)
		conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
		if err != nil {
			log.Printf("P2P: dial failed for %s: %v", addr, err)
			return err
		}
		p.conn = conn
		p.peerAddr = addr
		if err := p.handshake(); err != nil {
			log.Printf("P2P: handshake failed for %s: %v", addr, err)
			conn.Close()
			p.conn, p.peerAddr = nil, ""
			return err
		}
		connectedAddr := p.peerAddr // Save before any potential changes
		log.Printf("P2P: ✅ connected to %s", connectedAddr)
		return nil
	}
	log.Printf("P2P: connecting to %s (IPv4 preferred, 15s timeout)...", addr)
	conn, connectedAddr, err := dialPreferredIPv4(host, port, 15*time.Second)
	if err != nil {
		log.Printf("P2P: dial failed for %s: %v", addr, err)
		return err
	}
	p.conn = conn
	p.peerAddr = connectedAddr
	log.Printf("P2P: DEBUG - peerAddr set to %s before handshake", p.peerAddr)
	if err := p.handshake(); err != nil {
		log.Printf("P2P: handshake failed for %s: %v", addr, err)
		conn.Close()
		p.conn, p.peerAddr = nil, ""
		return err
	}
	log.Printf("P2P: DEBUG - peerAddr after handshake: %s, conn is nil: %v", p.peerAddr, p.conn == nil)
	// Log immediately with current peerAddr value
	log.Printf("P2P: ✅ connected to %s", p.peerAddr)
	return nil
}

// sendGetAddr sends a getaddr message (empty payload).
func (p *P2PClient) sendGetAddr() error {
	return p.writeMessage("getaddr", nil)
}

// parseAddrPayload parses a Bitcoin/Dogecoin "addr" message payload and returns "host:port" strings.
// Format: varint count, then per entry: timestamp(4) services(8) ip(16) port(2 BE).
// The 16-byte IP is either IPv4-mapped IPv6 (::ffff:a.b.c.d) or native IPv6. net.IP handles both.
// Note: Legacy "addr" has no TOR/onion (use BIP 155 addrv2) or domain names (IPs only).
func parseAddrPayload(payload []byte) ([]string, error) {
	r := bytes.NewReader(payload)
	count, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	var addrs []string
	const entrySize = 4 + 8 + 16 + 2 // time + services + IP + port
	for i := uint64(0); i < count && i < 1000; i++ {
		entry := make([]byte, entrySize)
		if _, err := io.ReadFull(r, entry); err != nil {
			break
		}
		ipBytes := entry[12:28] // 16-byte IP (IPv4-mapped or IPv6)
		if len(ipBytes) != 16 {
			continue
		}
		port := binary.BigEndian.Uint16(entry[28:30])
		host := net.IP(ipBytes).String()
		addrs = append(addrs, net.JoinHostPort(host, strconv.Itoa(int(port))))
	}
	return addrs, nil
}

// discoverPeersFromSeed connects to a seed, handshakes, sends getaddr, waits for addr, returns peer list.
func (p *P2PClient) discoverPeersFromSeed(seedHost string) ([]string, error) {
	port := strconv.Itoa(p.port)
	conn, connectedAddr, err := dialPreferredIPv4(seedHost, port, 10*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// Temporary client that uses this connection (so we don't touch p.conn)
	pc := &P2PClient{magic: p.magic, port: p.port}
	pc.conn = conn
	pc.peerAddr = connectedAddr
	if err := pc.sendVersion(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(15 * time.Second)
	var gotVerack bool
	for time.Now().Before(deadline) && !gotVerack {
		cmd, payload, err := pc.readMessage()
		if err != nil {
			return nil, err
		}
		log.Printf("P2P seed: ← received '%s' (%d bytes)", cmd, len(payload))
		switch cmd {
		case "version":
			// Parse peer info
			if len(payload) >= 80 {
				peerVersion := int32(binary.LittleEndian.Uint32(payload[0:4]))
				log.Printf("P2P seed: peer version=%d", peerVersion)
			}
			pc.writeMessage("verack", nil)
		case "verack":
			log.Printf("P2P seed: handshake complete")
			gotVerack = true
		case "ping":
			pc.writeMessage("pong", payload)
		}
	}
	if !gotVerack {
		return nil, fmt.Errorf("no verack from seed")
	}
	if err := pc.sendGetAddr(); err != nil {
		return nil, err
	}
	// Wait for "addr" message (or timeout)
	addrDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(addrDeadline) {
		cmd, payload, err := pc.readMessage()
		if err != nil {
			return nil, err
		}
		if cmd == "addr" && len(payload) > 0 {
			peers, err := parseAddrPayload(payload)
			if err != nil {
				return nil, err
			}
			return peers, nil
		}
	}
	return nil, fmt.Errorf("no addr response from seed")
}

// ensureConnected connects to a peer: try one peer at a time; only on error try the next; when no peers left, fetch more from seeds (each seed in turn).
// On first call (empty knownPeers), it loads cached good peers from disk.
func (p *P2PClient) ensureConnected() error {
	if p.conn != nil {
		return nil
	}

	// If user set a custom peer (e.g. testnet node when seed is down), try it first
	p.peersMux.Lock()
	override := p.optionalPeerOverride
	p.peersMux.Unlock()
	if override != "" {
		log.Printf("P2P: trying override peer %s...", override)
		if err := p.tryConnect(override); err == nil {
			if p.logFunc != nil {
				p.logFunc("P2P: connected to peer " + p.peerAddr)
			}
			log.Printf("P2P: connected to peer %s", p.peerAddr)
			p.savePeerToCache(p.peerAddr)
			return nil
		}
	}

	// Load cached peers from previous runs (only when knownPeers is empty)
	p.peersMux.Lock()
	noPeersYet := len(p.knownPeers) == 0
	p.peersMux.Unlock()
	if noPeersYet {
		p.loadCachedPeers()
	}

	var lastErr error
	for {
		// Get next peer to try (from current list)
		p.peersMux.Lock()
		havePeers := len(p.knownPeers) > 0
		var addr string
		if havePeers {
			addr = p.knownPeers[0]
			p.knownPeers = p.knownPeers[1:]
		}
		p.peersMux.Unlock()

		if havePeers {
			log.Printf("P2P: trying peer %s...", addr)
			connErr := p.tryConnect(addr)
			if connErr == nil {
				if p.logFunc != nil {
					p.logFunc("P2P: connected to peer " + p.peerAddr)
				}
				log.Printf("P2P: connected to peer %s", p.peerAddr)
				p.savePeerToCache(p.peerAddr)
				return nil
			}
			// Connection failed — mark as bad and fetch fresh peers from DNS seeds
			p.markBadPeer(addr)
			lastErr = connErr
			
			// If knownPeers list is now empty or has few peers left, fetch more from DNS seeds immediately
			p.peersMux.Lock()
			peersLeft := len(p.knownPeers)
			p.peersMux.Unlock()
			if peersLeft < 3 {
				log.Printf("P2P: only %d peer(s) left in list, fetching more from DNS seeds...", peersLeft)
				// Break out of peer-trying loop to trigger DNS seed resolution
				p.peersMux.Lock()
				p.knownPeers = nil // Clear remaining peers to force DNS fetch
				p.peersMux.Unlock()
			}
			continue
		}

		// No more peers in list — resolve DNS seeds to get peer IPs via A/AAAA records
		dnsResolved := false
		for _, seed := range p.seeds {
			ips, err := net.LookupHost(seed)
			if err != nil || len(ips) == 0 {
				continue
			}
			var dnsPeers []string
			for _, ip := range ips {
				dnsPeers = append(dnsPeers, net.JoinHostPort(ip, strconv.Itoa(p.port)))
			}
			// Filter out recently-failed peers
			dnsPeers = p.filterBadPeers(dnsPeers)
			if len(dnsPeers) == 0 {
				log.Printf("P2P: DNS lookup of %s returned IPs, but all are bad peers", seed)
				continue
			}
			rand.Shuffle(len(dnsPeers), func(i, j int) { dnsPeers[i], dnsPeers[j] = dnsPeers[j], dnsPeers[i] })
			p.peersMux.Lock()
			p.knownPeers = append(p.knownPeers, dnsPeers...)
			p.peersMux.Unlock()
			log.Printf("P2P: DNS lookup of %s returned %d IP(s) (%d after filtering bad peers)", seed, len(ips), len(dnsPeers))
			dnsResolved = true
		}
		if dnsResolved {
			continue
		}

		// DNS didn't return IPs — try P2P getaddr protocol on each seed
		fetched := false
		for _, seed := range p.seeds {
			log.Printf("P2P: no peers left, discovering from seed %s via getaddr...", seed)
			peers, err := p.discoverPeersFromSeed(seed)
			if err != nil {
				log.Printf("P2P: seed %s: %v", seed, err)
				lastErr = err
				continue
			}
			if len(peers) == 0 {
				continue
			}
			// Filter out recently-failed peers
			origCount := len(peers)
			peers = p.filterBadPeers(peers)
			if len(peers) == 0 {
				log.Printf("P2P: seed %s returned %d peer(s), but all are bad peers", seed, origCount)
				continue
			}
			rand.Shuffle(len(peers), func(i, j int) { peers[i], peers[j] = peers[j], peers[i] })
			if len(peers) > 50 {
				peers = peers[:50]
			}
			p.peersMux.Lock()
			p.knownPeers = append(p.knownPeers, peers...)
			p.peersMux.Unlock()
			log.Printf("P2P: discovered %d peer(s) from %s (%d after filtering bad peers)", origCount, seed, len(peers))
			fetched = true
			break
		}
		if fetched {
			continue
		}

		// No peers from any seed — fallback: connect directly to each seed host
		for _, host := range p.seeds {
			addr := net.JoinHostPort(host, strconv.Itoa(p.port))
			log.Printf("P2P: trying seed %s...", addr)
			connErr := p.tryConnect(addr)
			if connErr == nil {
				if p.logFunc != nil {
					p.logFunc("P2P: connected to seed " + p.peerAddr)
				}
				log.Printf("P2P: connected to seed %s", p.peerAddr)
				p.savePeerToCache(p.peerAddr)
				return nil
			}
			lastErr = connErr
		}
		if lastErr != nil {
			return fmt.Errorf("failed to connect to any peer or seed: %v", lastErr)
		}
		return fmt.Errorf("failed to connect to any peer or seed")
	}
}

// Disconnect closes the connection and clears state so the next ensureConnected() will reconnect.
// Removes the current peer from knownPeers so we try a different one next time. Safe to call from other packages.
func (p *P2PClient) Disconnect() {
	p.disconnect()
}

// disconnect closes the connection and clears state (internal use).
func (p *P2PClient) disconnect() {
	addr := p.peerAddr
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
		p.peerAddr = ""
	}
	if addr != "" {
		p.peersMux.Lock()
		for i, a := range p.knownPeers {
			if a == addr {
				p.knownPeers = append(p.knownPeers[:i], p.knownPeers[i+1:]...)
				break
			}
		}
		p.peersMux.Unlock()
	}
}

// writeMessage writes a Bitcoin-style P2P message.
func (p *P2PClient) writeMessage(command string, payload []byte) error {
	if len(command) > 12 {
		command = command[:12]
	}

	var hdr [24]byte
	copy(hdr[0:4], p.magic[:])
	copy(hdr[4:16], []byte(command))
	binary.LittleEndian.PutUint32(hdr[16:20], uint32(len(payload)))

	// checksum = first 4 bytes of double SHA256
	ck := sha256.Sum256(payload)
	ck = sha256.Sum256(ck[:])
	copy(hdr[20:24], ck[0:4])

	// Safety check: ensure connection exists before writing
	if p.conn == nil {
		return fmt.Errorf("P2P: cannot write message - not connected")
	}
	
	if _, err := p.conn.Write(hdr[:]); err != nil {
		p.disconnect()
		return err
	}
	if len(payload) > 0 {
		if _, err := p.conn.Write(payload); err != nil {
			p.disconnect()
			return err
		}
	}
	return nil
}

// readMessage reads a single P2P message.
func (p *P2PClient) readMessage() (string, []byte, error) {
	var hdr [24]byte
	if _, err := io.ReadFull(p.conn, hdr[:]); err != nil {
		// Don't disconnect on timeout/deadline errors (caller might be draining with deadline set)
		if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
			p.disconnect()
		}
		return "", nil, err
	}

	// Debug: Always log magic bytes for first message or when there's a mismatch
	receivedMagic := hdr[0:4]
	if !bytes.Equal(receivedMagic, p.magic[:]) {
		log.Printf("P2P DEBUG: Magic mismatch from %s", p.peerAddr)
		log.Printf("  Received: %02x %02x %02x %02x", receivedMagic[0], receivedMagic[1], receivedMagic[2], receivedMagic[3])
		log.Printf("  Expected: %02x %02x %02x %02x (%s)", p.magic[0], p.magic[1], p.magic[2], p.magic[3], p.network)
		return "", nil, fmt.Errorf("invalid magic in P2P header: got %02x %02x %02x %02x, expected %02x %02x %02x %02x (network: %s)", 
			receivedMagic[0], receivedMagic[1], receivedMagic[2], receivedMagic[3], 
			p.magic[0], p.magic[1], p.magic[2], p.magic[3],
			p.network)
	}

	cmdBytes := hdr[4:16]
	if i := bytes.IndexByte(cmdBytes, 0); i >= 0 {
		cmdBytes = cmdBytes[:i]
	}
	command := string(cmdBytes)

	length := binary.LittleEndian.Uint32(hdr[16:20])
	if length > 32*1024*1024 {
		return "", nil, fmt.Errorf("message too large: %d bytes", length)
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(p.conn, payload); err != nil {
			// Don't disconnect on timeout/deadline errors
			if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
				p.disconnect()
			}
			return "", nil, err
		}
	}

	// Verify checksum (best-effort)
	ck := sha256.Sum256(payload)
	ck = sha256.Sum256(ck[:])
	if !bytes.Equal(hdr[20:24], ck[0:4]) {
		return "", nil, fmt.Errorf("invalid checksum for %s", command)
	}

	return command, payload, nil
}

func (p *P2PClient) handshake() error {
	log.Printf("P2P: sending version message (protocol 70015, network: %s)", p.network)
	if err := p.sendVersion(); err != nil {
		return err
	}

	deadline := time.Now().Add(15 * time.Second)
	gotVerack := false

	for time.Now().Before(deadline) && !gotVerack {
		cmd, payload, err := p.readMessage()
		if err != nil {
			log.Printf("P2P: handshake read error from %s: %v", p.peerAddr, err)
			return err
		}

		log.Printf("P2P: ← received '%s' (%d bytes) from %s", cmd, len(payload), p.peerAddr)

		switch cmd {
		case "version":
			// Parse and store peer version info (for web UI) and log
			if len(payload) >= 80 {
				p.peerVersion = int32(binary.LittleEndian.Uint32(payload[0:4]))
				p.peerServices = binary.LittleEndian.Uint64(payload[4:12])
				if len(payload) > 80 {
					r := bytes.NewReader(payload[80:])
					if agentLen, err := readVarInt(r); err == nil && agentLen > 0 && agentLen < 256 {
						agentBytes := make([]byte, agentLen)
						if _, err := io.ReadFull(r, agentBytes); err == nil {
							p.peerUserAgent = string(agentBytes)
						}
					}
				}
				log.Printf("P2P: 📋 Peer info - version=%d, services=0x%x, user_agent='%s'", p.peerVersion, p.peerServices, p.peerUserAgent)
			}
			// Reply with verack
			log.Printf("P2P: → sending verack to %s", p.peerAddr)
			if err := p.writeMessage("verack", nil); err != nil {
				return err
			}
		case "verack":
			log.Printf("P2P: ✅ Handshake complete with %s", p.peerAddr)
			gotVerack = true
		case "ping":
			log.Printf("P2P: → sending pong to %s", p.peerAddr)
			if err := p.writeMessage("pong", payload); err != nil {
				return err
			}
		default:
			// Ignore other messages during handshake
			log.Printf("P2P: ignoring '%s' message during handshake", cmd)
		}
	}

	if !gotVerack {
		return fmt.Errorf("P2P handshake failed: no verack received within 15s")
	}
	
	// After handshake, briefly drain any immediate unsolicited messages (sendheaders, inv, addr, etc.)
	// Many peers send these automatically; we need to drain them before sending getheaders
	// Using 1 second timeout to ensure we consume all immediate messages
	drainDeadline := time.Now().Add(1 * time.Second)
	p.conn.SetReadDeadline(drainDeadline)
	defer func() {
		if p.conn != nil {
			p.conn.SetReadDeadline(time.Time{}) // Clear deadline
		}
	}()
	
	drainedCount := 0
	for time.Now().Before(drainDeadline) {
		cmd, payload, err := p.readMessage()
		if err != nil {
			// Timeout or EOF is fine - means no more unsolicited messages
			if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline") {
				break
			}
			// Other errors might be real issues, but continue anyway
			break
		}
		drainedCount++
		
		// Handle ping during drain (important!)
		if cmd == "ping" {
			p.writeMessage("pong", payload)
		}
		
		// Safety: don't drain forever
		if drainedCount > 10 {
			break
		}
	}
	
	if drainedCount > 0 {
		log.Printf("P2P: drained %d unsolicited messages, ready for getheaders", drainedCount)
	}
	
	return nil
}

// sendVersion sends a minimal version message.
func (p *P2PClient) sendVersion() error {
	var buf bytes.Buffer

	const protocolVersion int32 = 70015
	services := uint64(1) // NODE_NETWORK
	timestamp := time.Now().Unix()

	// version (int32)
	binary.Write(&buf, binary.LittleEndian, protocolVersion)
	// services (uint64)
	binary.Write(&buf, binary.LittleEndian, services)
	// time (int64)
	binary.Write(&buf, binary.LittleEndian, timestamp)

	// addr_recv (services + IP/port) - we don't know yet, use dummy
	binary.Write(&buf, binary.LittleEndian, services)
	buf.Write(make([]byte, 16)) // IPv6/IPv4-mapped IP (all zeros)
	binary.Write(&buf, binary.BigEndian, uint16(p.port))

	// addr_from (services + IP/port) - dummy
	binary.Write(&buf, binary.LittleEndian, services)
	buf.Write(make([]byte, 16))
	binary.Write(&buf, binary.BigEndian, uint16(0))

	// nonce (uint64)
	binary.Write(&buf, binary.LittleEndian, rand.Uint64())

	// user agent (varstr)
	userAgent := "/DogeLuckyMining:0.1.0/"
	writeVarInt(&buf, uint64(len(userAgent)))
	buf.Write([]byte(userAgent))

	// start_height (int32) - unknown, use 0
	binary.Write(&buf, binary.LittleEndian, int32(0))

	// relay flag (bool)
	buf.WriteByte(1)

	return p.writeMessage("version", buf.Bytes())
}

// GetTipHeader fetches headers (from our last known point or from genesis) and returns the best header.
func (p *P2PClient) GetTipHeader() (*BlockHeader, error) {
	if err := p.ensureConnected(); err != nil {
		return nil, err
	}

	// Throttle header requests (testnet: 2s so we follow fast blocks; mainnet: 15s)
	if p.headerRefreshInterval > 0 && time.Since(p.lastHeadersReq) < p.headerRefreshInterval && p.bestHeader != nil {
		return p.bestHeader, nil
	}

	if err := p.requestHeaders(); err != nil {
		return nil, err
	}
	if p.bestHeader == nil {
		return nil, fmt.Errorf("no headers received from peers")
	}
	return p.bestHeader, nil
}

const maxHeadersPerMessage = 2000 // Bitcoin/Dogecoin send at most 2000 headers per "headers" message

// sendGetHeaders sends a single getheaders message with current best known block (or checkpoint/genesis).
func (p *P2PClient) sendGetHeaders() error {
	var buf bytes.Buffer
	const protocolVersion int32 = 70015

	binary.Write(&buf, binary.LittleEndian, protocolVersion)
	writeVarInt(&buf, 1)

	locatorBytes := make([]byte, 32)
	if p.bestHeader != nil {
		copy(locatorBytes, p.bestHeader.Hash[:]) // internal order, no reverse
	} else if p.hasStartCheckpoint {
		copy(locatorBytes, p.startLocatorHash[:]) // checkpoints stored in wire order, send as-is
	}
	// else genesis: locatorBytes stays zero
	buf.Write(locatorBytes)
	buf.Write(make([]byte, 32)) // stop hash = zero (no stop)

	return p.writeMessage("getheaders", buf.Bytes())
}

// requestHeaders fetches headers from the peer and keeps requesting the next batch until the chain tip.
// Starting from checkpoint (or genesis), it gets headers in batches of up to 2000 until the peer
// returns fewer than 2000 (meaning we're at the tip).
func (p *P2PClient) requestHeaders() error {
	p.lastHeadersReq = time.Now()
	deadline := time.Now().Add(120 * time.Second) // allow time for multiple batches to reach tip

	for time.Now().Before(deadline) {
		if err := p.sendGetHeaders(); err != nil {
			return err
		}

		// Wait for "headers" message (handle ping/pong and other messages)
		var gotHeaders bool
		for time.Now().Before(deadline) {
			cmd, payload, err := p.readMessage()
			if err != nil {
				return err
			}
			switch cmd {
			case "headers":
				count, err := p.processHeaders(payload)
				if err != nil {
					return err
				}
				gotHeaders = true
				// If we got fewer than max, we're at the tip; stop syncing
				if count < maxHeadersPerMessage {
					if p.logFunc != nil && p.bestHeader != nil {
						p.logFunc(fmt.Sprintf("P2P: synced to chain tip at height %d", p.bestHeight))
					}
					return nil
				}
				// Log progress and request the next batch
				if p.logFunc != nil && p.bestHeader != nil {
					p.logFunc(fmt.Sprintf("P2P: received %d headers, tip now %d; fetching next batch...", count, p.bestHeight))
				}
				break
			case "ping":
				if err := p.writeMessage("pong", payload); err != nil {
					return err
				}
			default:
				// ignore other messages
			}
			if gotHeaders {
				break
			}
		}
		if !gotHeaders {
			return fmt.Errorf("timeout waiting for headers response")
		}
	}

	return fmt.Errorf("timeout syncing headers to tip")
}

// Dogecoin VERSION_AUXPOW (1 << 8): blocks with this flag in version include auxpow data after the 80-byte header.
const versionAuxPow int32 = 0x100

// processHeaders parses a headers message and updates bestHeader/bestHeight.
// Dogecoin "headers" message uses CBlock serialization: 80-byte (pure) header, then optional AuxPoW
// (for merge-mined blocks, version & 0x100), then varint txn_count (0). We skip AuxPoW so the stream stays aligned.
// It also validates chain linkage: each header's prevBlock must match the previous header's hash (or our locator).
// It returns the number of headers processed so the caller can detect when we've reached the tip (count < 2000).
func (p *P2PClient) processHeaders(payload []byte) (int, error) {
	r := bytes.NewReader(payload)
	count, err := readVarInt(r)
	if err != nil {
		return 0, fmt.Errorf("failed to read headers count: %v", err)
	}

	if count == 0 {
		return 0, nil
	}

	// First header height: bestHeight+1, or startHeight+1 if we started from a checkpoint
	height := p.bestHeight
	if p.bestHeader == nil && p.hasStartCheckpoint {
		height = p.startHeight
	}
	var lastHeader *BlockHeader
	n := int(count)

	// Expected previous block hash for chain validation. prevBlock on the wire is in wire/internal order.
	// Our computed Hash and checkpoint hashes are stored in that order.
	var expectedPrev [32]byte
	if p.bestHeader != nil {
		copy(expectedPrev[:], p.bestHeader.Hash[:])
	} else if p.hasStartCheckpoint {
		copy(expectedPrev[:], p.startLocatorHash[:]) // checkpoints in wire order, no reverse
	} else {
		// Starting from genesis: first header (block 1) has prevBlock = genesis block hash
		copy(expectedPrev[:], p.genesisHashWire[:])
	}

	for i := uint64(0); i < count; i++ {
		headerBytes := make([]byte, 80)
		if _, err := io.ReadFull(r, headerBytes); err != nil {
			return 0, fmt.Errorf("failed to read header bytes: %v", err)
		}

		version := int32(binary.LittleEndian.Uint32(headerBytes[0:4]))
		// Dogecoin: after 80-byte pure header, CBlockHeader may have AuxPoW (merge-mined blocks since ~block 371337)
		if (version & versionAuxPow) != 0 {
			if err := skipAuxPow(r); err != nil {
				return 0, fmt.Errorf("failed to skip auxpow at height %d: %v", height+1, err)
			}
		}

		// txn_count (varint) - CBlock.vtx size, always 0 in headers message
		if _, err := readVarInt(r); err != nil {
			return 0, fmt.Errorf("failed to read txn_count: %v", err)
		}

		h := &BlockHeader{}
		height++
		h.Height = height
		h.Version = version
		copy(h.PrevBlock[:], headerBytes[4:36])
		copy(h.MerkleRoot[:], headerBytes[36:68])
		h.Timestamp = binary.LittleEndian.Uint32(headerBytes[68:72])
		h.Bits = binary.LittleEndian.Uint32(headerBytes[72:76])
		h.Nonce = binary.LittleEndian.Uint32(headerBytes[76:80])

	// Chain validation (like the wallet): prevBlock must link to previous header or our locator
	if !bytes.Equal(h.PrevBlock[:], expectedPrev[:]) {
		// Debug: show what checkpoint we started from
		debugInfo := ""
		if p.hasStartCheckpoint && lastHeader == nil {
			debugInfo = fmt.Sprintf(" [started from checkpoint height %d, hash %s]", 
				p.startHeight, hex.EncodeToString(p.startLocatorHash[:]))
		}
		return 0, fmt.Errorf("non-continuous headers: at height %d prevBlock %s does not link (expected %s)%s",
			height, hex.EncodeToString(h.PrevBlock[:]), hex.EncodeToString(expectedPrev[:]), debugInfo)
	}

		// Block hash for chain linkage (prevBlock) is double-SHA256 of header (Dogecoin GetHash()), not Scrypt
		hash := blockHashChain(headerBytes)
		if len(hash) == 32 {
			copy(h.Hash[:], hash)
		}

		// Next header's prevBlock must equal this block's hash (same natural byte order on wire)
		copy(expectedPrev[:], h.Hash[:])
		lastHeader = h
	}

	if lastHeader != nil {
		p.bestHeader = lastHeader
		p.bestHeight = lastHeader.Height
		// Only log when we've reached the actual tip (last batch); during sync we get 2000 headers per batch and skip logging to avoid flood
		if n < maxHeadersPerMessage {
			hashDisplay := HashToDisplayHex(p.bestHeader.Hash[:])
			log.Printf("P2P: updated tip to height %d (hash %s)", p.bestHeight, hashDisplay)
			if p.logFunc != nil {
				peerInfo := p.peerAddr
				if peerInfo == "" {
					peerInfo = "peer"
				}
				p.logFunc(fmt.Sprintf("P2P: %s returned tip height %d, hash %s", peerInfo, p.bestHeight, hashDisplay))
			}
		}
	}

	return n, nil
}

// skipAuxPow skips a CAuxPow structure in the stream (Dogecoin merge-mining data after the 80-byte header).
// CAuxPow = CMerkleTx (tx, hashBlock, vMerkleBranch, nIndex) + vChainMerkleBranch + nChainIndex + parentBlock (80 bytes).
func skipAuxPow(r io.Reader) error {
	rr, ok := r.(*bytes.Reader)
	if !ok {
		return fmt.Errorf("skipAuxPow: need *bytes.Reader")
	}
	// Skip CMerkleTx: tx (CTransaction), hashBlock (32), vMerkleBranch (varint + n*32), nIndex (4)
	if err := skipTransaction(rr); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, rr, 32); err != nil {
		return err
	}
	nBranch, err := readVarInt(rr)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, rr, int64(nBranch*32)); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, rr, 4); err != nil {
		return err
	}
	// vChainMerkleBranch: varint + n*32
	nChain, err := readVarInt(rr)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, rr, int64(nChain*32)); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, rr, 4); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, rr, 80); err != nil {
		return err
	}
	return nil
}

// skipTransaction skips a Bitcoin/Dogecoin CTransaction in the stream.
func skipTransaction(r *bytes.Reader) error {
	if _, err := io.CopyN(io.Discard, r, 4); err != nil {
		return err
	}
	nVin, err := readVarInt(r)
	if err != nil {
		return err
	}
	for i := uint64(0); i < nVin; i++ {
		if _, err := io.CopyN(io.Discard, r, 36); err != nil {
			return err
		}
		scriptLen, err := readVarInt(r)
		if err != nil {
			return err
		}
		if _, err := io.CopyN(io.Discard, r, int64(scriptLen)); err != nil {
			return err
		}
		if _, err := io.CopyN(io.Discard, r, 4); err != nil {
			return err
		}
	}
	nVout, err := readVarInt(r)
	if err != nil {
		return err
	}
	for i := uint64(0); i < nVout; i++ {
		if _, err := io.CopyN(io.Discard, r, 8); err != nil {
			return err
		}
		scriptLen, err := readVarInt(r)
		if err != nil {
			return err
		}
		if _, err := io.CopyN(io.Discard, r, int64(scriptLen)); err != nil {
			return err
		}
	}
	if _, err := io.CopyN(io.Discard, r, 4); err != nil {
		return err
	}
	return nil
}

// maintainBroadcastConnections ensures we have multiple active peer connections for block broadcasting
func (p *P2PClient) maintainBroadcastConnections() {
	// Add panic recovery to prevent crashes
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ Panic in maintainBroadcastConnections (recovered): %v", r)
		}
	}()
	
	p.broadcastMux.Lock()
	defer p.broadcastMux.Unlock()
	
	// Close dead connections (non-blocking checks only)
	for addr, conn := range p.broadcastConns {
		if conn == nil {
			delete(p.broadcastConns, addr)
			continue
		}
		// Skip health check during maintenance - just keep connections
		// Health will be verified during actual broadcast
	}
	
	// Add new connections if below max
	if len(p.broadcastConns) < p.maxBroadcastPeers {
		p.peersMux.Lock()
		availablePeers := make([]string, len(p.knownPeers))
		copy(availablePeers, p.knownPeers)
		p.peersMux.Unlock()
		
		// Filter bad peers
		availablePeers = p.filterBadPeers(availablePeers)
		
		needed := p.maxBroadcastPeers - len(p.broadcastConns)
		for i := 0; i < needed && i < len(availablePeers); i++ {
			addr := availablePeers[i]
			if p.broadcastConns[addr] != nil {
				continue // already connected
			}
			
			// Validate address format before splitting
			parts := strings.Split(addr, ":")
			if len(parts) != 2 {
				log.Printf("⚠️ Invalid peer address format: %s", addr)
				continue
			}
			
			// Try to connect (with error handling)
			conn, _, err := dialPreferredIPv4(parts[0], parts[1], 5*time.Second)
			if err == nil {
				p.broadcastConns[addr] = conn
				log.Printf("P2P: added broadcast peer %s (total: %d)", addr, len(p.broadcastConns))
			} else {
				log.Printf("P2P: failed to connect to broadcast peer %s: %v", addr, err)
			}
		}
	}
}

// sendBlockToPeer sends a block message to a specific connection
func (p *P2PClient) sendBlockToPeer(conn net.Conn, addr string, blockBytes []byte) error {
	// Add panic recovery
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ Panic in sendBlockToPeer to %s (recovered): %v", addr, r)
		}
	}()
	
	if conn == nil {
		return fmt.Errorf("connection is nil")
	}
	if len(blockBytes) == 0 {
		return fmt.Errorf("block bytes empty")
	}
	
	// Write block message
	payload := new(bytes.Buffer)
	magic := p.magic
	payload.Write(magic[:])
	
	cmd := "block"
	cmdBytes := make([]byte, 12)
	copy(cmdBytes, cmd)
	payload.Write(cmdBytes)
	
	length := uint32(len(blockBytes))
	lengthBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(lengthBuf, length)
	payload.Write(lengthBuf)
	
	checksum := sha256.Sum256(blockBytes)
	checksum = sha256.Sum256(checksum[:])
	payload.Write(checksum[:4])
	
	payload.Write(blockBytes)
	
	// Set write deadline
	if c, ok := conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		defer c.SetWriteDeadline(time.Time{})
	}
	
	_, err := conn.Write(payload.Bytes())
	return err
}

// BroadcastBlock sends a mined block to ALL connected peers for fast network propagation
func (p *P2PClient) BroadcastBlock(block *Block) error {
	// Add panic recovery to prevent crashes during block broadcast
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ Panic in BroadcastBlock (recovered): %v", r)
		}
	}()
	
	if block == nil {
		return fmt.Errorf("block is nil")
	}
	
	// Serialize block once
	blockHex, err := block.Serialize()
	if err != nil {
		return fmt.Errorf("failed to serialize block: %v", err)
	}
	blockBytes, err := hex.DecodeString(blockHex)
	if err != nil {
		return fmt.Errorf("failed to decode block hex: %v", err)
	}
	
	// Ensure we have broadcast connections (with timeout to prevent hanging)
	done := make(chan bool, 1)
	go func() {
		p.maintainBroadcastConnections()
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		log.Printf("⚠️ maintainBroadcastConnections timeout, continuing with existing connections")
	}
	
	// Send to primary connection
	sentToPrimary := false
	if p.conn != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("⚠️ Panic sending to primary peer (recovered): %v", r)
				}
			}()
			if err := p.writeMessage("block", blockBytes); err == nil {
				sentToPrimary = true
				log.Printf("P2P: sent block to primary peer %s", p.peerAddr)
			} else {
				log.Printf("P2P: failed to send to primary peer %s: %v", p.peerAddr, err)
			}
		}()
	}
	
	// Send to all broadcast connections
	p.broadcastMux.RLock()
	broadcastAddrs := make([]string, 0, len(p.broadcastConns))
	for addr := range p.broadcastConns {
		broadcastAddrs = append(broadcastAddrs, addr)
	}
	p.broadcastMux.RUnlock()
	
	successCount := 0
	if sentToPrimary {
		successCount++
	}
	
	for _, addr := range broadcastAddrs {
		func(peerAddr string) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("⚠️ Panic broadcasting to %s (recovered): %v", peerAddr, r)
				}
			}()
			
			p.broadcastMux.RLock()
			conn := p.broadcastConns[peerAddr]
			p.broadcastMux.RUnlock()
			
			if conn == nil {
				return
			}
			
			if err := p.sendBlockToPeer(conn, peerAddr, blockBytes); err == nil {
				successCount++
				log.Printf("P2P: sent block to broadcast peer %s", peerAddr)
			} else {
				log.Printf("P2P: failed to send block to %s: %v", peerAddr, err)
				// Remove failed connection (with safety check)
				p.broadcastMux.Lock()
				if conn := p.broadcastConns[peerAddr]; conn != nil {
					conn.Close()
				}
				delete(p.broadcastConns, peerAddr)
				p.broadcastMux.Unlock()
			}
		}(addr)
	}
	
	log.Printf("P2P: ✅ Block broadcast complete - sent to %d peer(s) (%d bytes each)", successCount, len(blockBytes))
	if p.logFunc != nil {
		p.logFunc(fmt.Sprintf("P2P: Block broadcast to %d peer(s)", successCount))
	}
	
	// Check for rejection on primary connection
	if p.conn != nil && sentToPrimary {
		if c, ok := p.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
			c.SetReadDeadline(time.Now().Add(3 * time.Second))
			defer func() {
				if c2, ok := p.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
					c2.SetReadDeadline(time.Time{}) // clear deadline
				}
			}()
		}
		for i := 0; i < 3; i++ {
			cmd, payload, readErr := p.readMessage()
			if readErr != nil {
				break
			}
			if cmd == "reject" {
				msgType, code, reason, _, parseErr := parseRejectPayload(payload)
				if parseErr == nil && msgType == "block" {
					return fmt.Errorf("block rejected by peer: %s (reject code 0x%02x)", reason, code)
				}
			}
			if cmd == "ping" {
				_ = p.writeMessage("pong", payload)
				continue
			}
			break
		}
	}
	
	if successCount == 0 {
		return fmt.Errorf("failed to broadcast block to any peer")
	}
	
	return nil
}

// blockHashChain returns the block hash used for chain linkage (prevBlock).
// Dogecoin uses GetHash() = double-SHA256(header), same as Bitcoin — not Scrypt.
func blockHashChain(header []byte) []byte {
	h := sha256.Sum256(header)
	final := sha256.Sum256(h[:])
	return final[:]
}

// dogeHeaderHash computes the Dogecoin PoW hash: scrypt(header80, header80) -> 32 bytes (no SHA256).
func dogeHeaderHash(header []byte) []byte {
	if len(header) != 80 {
		panic(fmt.Sprintf("dogeHeaderHash requires 80-byte header, got %d", len(header)))
	}
	hash, err := scrypt.Key(header, header, 1024, 1, 1, 32)
	if err != nil {
		panic(fmt.Sprintf("scrypt error: %v", err))
	}
	return hash
}

// readVarInt reads a Bitcoin-style variable length integer from r.
func readVarInt(r *bytes.Reader) (uint64, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	switch b {
	case 0xfd:
		var v uint16
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return 0, err
		}
		return uint64(v), nil
	case 0xfe:
		var v uint32
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return 0, err
		}
		return uint64(v), nil
	case 0xff:
		var v uint64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return 0, err
		}
		return v, nil
	default:
		return uint64(b), nil
	}
}

// readVarStr reads a Bitcoin-style var_str (var_int length + bytes) from r.
func readVarStr(r *bytes.Reader) ([]byte, error) {
	n, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	if n > 1<<20 {
		return nil, fmt.Errorf("var_str too long: %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// parseRejectPayload parses a BIP 61 "reject" message payload. Returns message type, code, reason, and optional hash (for block/tx).
func parseRejectPayload(payload []byte) (msgType string, code uint8, reason string, _ []byte, err error) {
	r := bytes.NewReader(payload)
	msgTypeB, err := readVarStr(r)
	if err != nil {
		return "", 0, "", nil, err
	}
	msgType = string(msgTypeB)
	if err := binary.Read(r, binary.LittleEndian, &code); err != nil {
		return msgType, 0, "", nil, err
	}
	reasonB, err := readVarStr(r)
	if err != nil {
		return msgType, code, "", nil, err
	}
	reason = string(reasonB)
	if msgType == "block" || msgType == "tx" {
		hash := make([]byte, 32)
		if _, err := io.ReadFull(r, hash); err == nil {
			return msgType, code, reason, hash, nil
		}
	}
	return msgType, code, reason, nil, nil
}

