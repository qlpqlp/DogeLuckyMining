let ws = null;
let wsReconnectDelay = 3000; // WebSocket reconnect delay with exponential backoff
let lastLogMessageCount = 0; // Track how many log messages we've already displayed
let showHashAttempts = true; // Default to showing hash attempts (checkbox is checked by default)
// Per-network payout addresses so switching mainnet/testnet keeps both
let storedPayoutMainnet = '';
let storedPayoutTestnet = '';
// Network currently shown in the payout input (so we save to the right slot when switching)
let currentConfigNetwork = 'mainnet';

function formatHashRate(hashes) {
    if (hashes < 1000) {
        return hashes.toFixed(2) + ' H/s';
    } else if (hashes < 1000000) {
        return (hashes / 1000).toFixed(2) + ' KH/s';
    } else if (hashes < 1000000000) {
        return (hashes / 1000000).toFixed(2) + ' MH/s';
    } else {
        return (hashes / 1000000000).toFixed(2) + ' GH/s';
    }
}

function formatNumber(num) {
    return num.toLocaleString();
}

function formatUptime(startTime) {
    if (!startTime) return '00:00:00';
    
    const start = new Date(startTime);
    const now = new Date();
    const diff = Math.floor((now - start) / 1000);
    
    const hours = Math.floor(diff / 3600);
    const minutes = Math.floor((diff % 3600) / 60);
    const seconds = diff % 60;
    
    return `${String(hours).padStart(2, '0')}:${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}`;
}

function addLogEntry(message, type = 'info') {
    const log = document.getElementById('log');
    const entry = document.createElement('div');
    entry.className = `log-entry ${type}`;
    entry.textContent = `[${new Date().toLocaleTimeString()}] ${message}`;
    log.appendChild(entry);
    log.scrollTop = log.scrollHeight;
}

function updateStats(stats) {
    document.getElementById('hashRate').textContent = formatHashRate(stats.hashRate || 0);
    document.getElementById('totalHashes').textContent = formatNumber(stats.totalHashes || 0);
    document.getElementById('blocksFound').textContent = formatNumber(stats.blocksFound || 0);
    document.getElementById('uptime').textContent = formatUptime(stats.startTime);
    
    // Update network badge
    const networkBadge = document.getElementById('networkBadge');
    if (networkBadge && stats.network) {
        const network = stats.network.toLowerCase();
        networkBadge.textContent = network === 'testnet' ? 'Testnet' : 'Mainnet';
        networkBadge.className = 'network-badge ' + network;
    }
    
    if (stats.currentBlock) {
        document.getElementById('blockHeight').textContent = stats.currentBlock.height || '-';
        // Format difficulty with commas for readability (no scientific notation)
        const difficulty = stats.currentBlock.difficulty || 0;
        const diffEl = document.getElementById('blockDifficulty');
        if (difficulty > 0) {
            // Use toLocaleString with max 2 decimal places for large numbers
            const diffStr = difficulty.toLocaleString('en-US', { 
                minimumFractionDigits: 0,
                maximumFractionDigits: difficulty >= 1 ? 2 : 6
            });
            diffEl.textContent = diffStr;
            diffEl.title = difficulty.toLocaleString('en-US', { maximumFractionDigits: 6 });
        } else {
            diffEl.textContent = '0';
            diffEl.title = '0';
        }
        // Target in status bar (ellipsis); full value on hover
        const target = stats.currentBlock.target || '-';
        document.getElementById('blockTarget').textContent = target;
        document.getElementById('targetBar').title = target;
    }
    // Expected time to find one block at current difficulty + hashrate
    const expectedSec = stats.expectedSecondsPerBlock || 0;
    const expectedEl = document.getElementById('expectedTimeToBlock');
    if (expectedSec > 0) {
        if (expectedSec >= 3600) {
            expectedEl.textContent = '~' + (expectedSec / 3600).toFixed(1) + ' h';
        } else if (expectedSec >= 60) {
            expectedEl.textContent = '~' + Math.round(expectedSec / 60) + ' min';
        } else {
            expectedEl.textContent = '~' + Math.round(expectedSec) + ' s';
        }
        expectedEl.title = 'On average at current difficulty and hashrate. Actual time varies by luck.';
    } else {
        expectedEl.textContent = '-';
        expectedEl.title = 'Start mining to see estimated time to block.';
    }
    // Time spent on last block attempt (e.g. "0.5 s" or "2.3 s")
    const attemptSec = stats.blockAttemptSeconds;
    const attemptEl = document.getElementById('blockAttemptSeconds');
    if (attemptSec != null && attemptSec > 0) {
        if (attemptSec < 1) {
            attemptEl.textContent = (attemptSec * 1000).toFixed(0) + ' ms';
        } else if (attemptSec < 60) {
            attemptEl.textContent = attemptSec.toFixed(2) + ' s';
        } else {
            attemptEl.textContent = (attemptSec / 60).toFixed(1) + ' min';
        }
        attemptEl.title = 'Time spent on the last block mining attempt.';
    } else {
        attemptEl.textContent = '-';
        attemptEl.title = 'Time on current/last block attempt.';
    }
    
    // P2P peer and peer details (version, subversion, services)
    const p2pPeerWrap = document.getElementById('p2pPeerWrap');
    const p2pPeerEl = document.getElementById('p2pPeer');
    const p2pPeerDetailsEl = document.getElementById('p2pPeerDetails');
    if (stats.p2pPeer) {
        p2pPeerWrap.style.display = '';
        p2pPeerEl.textContent = stats.p2pPeer;
        if (p2pPeerDetailsEl && stats.p2pPeerInfo) {
            const pi = stats.p2pPeerInfo;
            const parts = [];
            if (pi.version) parts.push('version ' + pi.version);
            if (pi.userAgent) parts.push('subversion ' + pi.userAgent);
            if (pi.services) parts.push('services 0x' + pi.services.toString(16));
            p2pPeerDetailsEl.textContent = parts.length ? parts.join(' · ') : '';
        } else if (p2pPeerDetailsEl) {
            p2pPeerDetailsEl.textContent = '';
        }
    } else {
        p2pPeerWrap.style.display = 'none';
    }
    
    // Mining log is now refreshed from /api/logs poll (same as console output)
    
    // Update status
    const statusIndicator = document.getElementById('statusIndicator');
    const statusText = document.getElementById('statusText');
    
    if (stats.status === 'running') {
        statusIndicator.classList.add('active');
        statusText.textContent = 'Mining Active';
    } else {
        statusIndicator.classList.remove('active');
        statusText.textContent = 'Stopped';
    }
}

function updateMiningLog(messages) {
    if (!messages || messages.length === 0) {
        return; // Don't clear log if no messages
    }
    
    const log = document.getElementById('log');
    
    // Only add new messages that we haven't displayed yet
    // This preserves client-side logs (like "Mining started" from addLogEntry)
    const newMessages = messages.slice(lastLogMessageCount);
    
    newMessages.forEach(msg => {
        // Truncate very long messages client-side (defense in depth)
        if (msg.length > 500) {
            msg = msg.substring(0, 500) + '... (truncated)';
        }
        
        // Skip only the frequent hash attempt messages (🔍) if toggle is off
        // Don't filter completion/start messages - those should always show
        if (!showHashAttempts && msg.includes('🔍')) {
            return;
        }
        
        const entry = document.createElement('div');
        entry.className = 'log-entry';
        // Determine log type based on message content
        if (msg.includes('🎉') || msg.includes('BLOCK FOUND') || msg.includes('✅')) {
            entry.className += ' success';
        } else if (msg.includes('❌') || msg.includes('Error')) {
            entry.className += ' error';
        } else if (msg.includes('🔍') || msg.includes('⛏️') || msg.includes('🚀')) {
            entry.className += ' info';
        }
        entry.textContent = `[${new Date().toLocaleTimeString()}] ${msg}`;
        log.appendChild(entry);
    });
    
    // Update the count of messages we've displayed
    lastLogMessageCount = messages.length;
    
    // Aggressive cleanup: Limit log to last 100 entries
    const entries = log.querySelectorAll('.log-entry');
    if (entries.length > 100) {
        // Remove oldest entries to keep exactly 100
        const toRemove = entries.length - 100;
        for (let i = 0; i < toRemove; i++) {
            if (entries[i] && entries[i].parentNode) {
                entries[i].remove();
            }
        }
    }
    
    // Auto-scroll to bottom (show newest)
    log.scrollTop = log.scrollHeight;
}

// Periodic log cleanup to prevent memory leaks (runs every 30 seconds)
setInterval(function cleanupLogs() {
    const log = document.getElementById('log');
    if (!log) return;
    
    const entries = log.querySelectorAll('.log-entry');
    // If log somehow grew beyond limit, aggressively prune
    if (entries.length > 150) {
        const toRemove = entries.length - 100;
        for (let i = 0; i < toRemove; i++) {
            if (entries[i] && entries[i].parentNode) {
                entries[i].remove();
            }
        }
        console.log(`Cleaned up ${toRemove} old log entries`);
    }
}, 30000); // Every 30 seconds

function connectWebSocket() {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const wsUrl = `${protocol}//${window.location.host}/ws`;
    
    ws = new WebSocket(wsUrl);
    
    ws.onopen = () => {
        addLogEntry('WebSocket connected', 'success');
        wsReconnectDelay = 3000; // Reset backoff on successful connection
    };
    
    ws.onmessage = (event) => {
        try {
            const stats = JSON.parse(event.data);
            updateStats(stats);
        } catch (e) {
            console.error('Error parsing stats:', e);
        }
    };
    
    ws.onerror = (error) => {
        addLogEntry('WebSocket error', 'error');
        console.error('WebSocket error:', error);
    };
    
    ws.onclose = () => {
        addLogEntry('WebSocket disconnected. Reconnecting in ' + (wsReconnectDelay / 1000) + 's...', 'error');
        setTimeout(connectWebSocket, wsReconnectDelay);
        wsReconnectDelay = Math.min(wsReconnectDelay * 2, 60000); // Exponential backoff up to 60s
    };
}

function startMining() {
    fetch('/api/start', { method: 'POST' })
        .then(response => {
            if (response.ok) {
                addLogEntry('Mining started', 'success');
                document.getElementById('startBtn').disabled = true;
                document.getElementById('stopBtn').disabled = false;
                // Sync log counter: fetch current log length so we only show new messages going forward
                setTimeout(() => {
                    fetch('/api/stats')
                        .then(response => response.json())
                        .then(stats => {
                            if (stats.logMessages) {
                                lastLogMessageCount = stats.logMessages.length;
                                updateMiningLog(stats.logMessages);
                            }
                        })
                        .catch(err => console.error('Error refreshing log:', err));
                }, 500);
            } else {
                response.text().then(text => {
                    addLogEntry('Failed to start mining: ' + text, 'error');
                });
            }
        })
        .catch(error => {
            addLogEntry('Error starting mining: ' + error.message, 'error');
        });
}

function stopMining() {
    const stopBtn = document.getElementById('stopBtn');
    const startBtn = document.getElementById('startBtn');
    const originalLabel = stopBtn.textContent;
    stopBtn.textContent = 'Stopping…';
    stopBtn.disabled = true;

    fetch('/api/stop', { method: 'POST' })
        .then(response => {
            if (response.ok) {
                addLogEntry('Mining stopped', 'info');
                startBtn.disabled = false;
                stopBtn.textContent = originalLabel;
                stopBtn.disabled = true;
                // Refresh stats to get log messages (will append new ones)
                setTimeout(() => {
                    fetch('/api/stats')
                        .then(response => response.json())
                        .then(stats => {
                            if (stats.logMessages) {
                                updateMiningLog(stats.logMessages);
                            }
                        })
                        .catch(err => console.error('Error refreshing log:', err));
                }, 500);
            } else {
                addLogEntry('Failed to stop mining', 'error');
                stopBtn.textContent = originalLabel;
                stopBtn.disabled = false;
            }
        })
        .catch(error => {
            addLogEntry('Error stopping mining: ' + error.message, 'error');
            stopBtn.textContent = originalLabel;
            stopBtn.disabled = false;
        });
}

function loadInitialStats() {
    fetch('/api/stats')
        .then(response => response.json())
        .then(stats => {
            updateStats(stats);
            if (stats.status === 'running') {
                document.getElementById('startBtn').disabled = true;
                document.getElementById('stopBtn').disabled = false;
            }
            // Initialize log message counter
            if (stats.logMessages) {
                lastLogMessageCount = stats.logMessages.length;
            }
        })
        .catch(error => {
            console.error('Error loading stats:', error);
        });
}

// Configuration functions
function loadConfig() {
    fetch('/api/config')
        .then(response => response.json())
        .then(config => {
            const network = config.network || 'mainnet';
            const networkEl = document.getElementById('network');
            if (networkEl) networkEl.value = network;
            const connectionMode = config.connectionMode || 'rpc';
            const connectionModeEl = document.getElementById('connectionMode');
            if (connectionModeEl) {
                connectionModeEl.value = connectionMode;
                toggleRpcFieldsRequired(connectionMode);
                toggleRpcFieldsVisibility(connectionMode);
                toggleTestConnectionButton(connectionMode);
                toggleP2POptionsVisibility(connectionMode);
            }
            document.getElementById('rpcUrl').value = config.rpcUrl || '';
            updateRpcUrlPlaceholder(network);
            // Load per-network payout addresses (fallback to payoutAddr for old configs)
            storedPayoutMainnet = (config.payoutAddrMainnet !== undefined && config.payoutAddrMainnet !== null) ? config.payoutAddrMainnet : (config.payoutAddr || '');
            storedPayoutTestnet = (config.payoutAddrTestnet !== undefined && config.payoutAddrTestnet !== null) ? config.payoutAddrTestnet : '';
            currentConfigNetwork = network;
            updateNetworkDependentFields(network);
            document.getElementById('rpcUser').value = config.rpcUser || '';
            document.getElementById('rpcPass').value = config.rpcPass || '';
            document.getElementById('payoutAddr').value = network === 'testnet' ? storedPayoutTestnet : storedPayoutMainnet;
            const cp = config.p2pCheckpoint !== undefined ? config.p2pCheckpoint : '';
            const mainnetChk = document.getElementById('p2pCheckpointMainnet');
            const testnetChk = document.getElementById('p2pCheckpointTestnet');
            if (mainnetChk) mainnetChk.value = ['','0','3043797','5526282','6024440'].includes(cp) ? cp : '6024440';
            if (testnetChk) testnetChk.value = ['','0','38150909'].includes(cp) ? cp : '38150909';
            // Peer overrides (network-specific)
            const poMainnet = document.getElementById('p2pPeerOverrideMainnet');
            const poTestnet = document.getElementById('p2pPeerOverrideTestnet');
            if (poMainnet) poMainnet.value = (config.p2pPeerOverrideMainnet !== undefined && config.p2pPeerOverrideMainnet !== null) ? config.p2pPeerOverrideMainnet : '';
            if (poTestnet) poTestnet.value = (config.p2pPeerOverrideTestnet !== undefined && config.p2pPeerOverrideTestnet !== null) ? config.p2pPeerOverrideTestnet : '';
            // Mining performance settings (fallbacks match first-run defaults: gpu, 64, extreme, 1M)
            document.getElementById('deviceType').value = config.deviceType || 'gpu';
            document.getElementById('threadCount').value = config.threadCount || 64;
            document.getElementById('miningIntensity').value = config.miningIntensity || 'extreme';
            document.getElementById('maxHashes').value = config.maxHashes || 1000000;
            // Nonce search strategy settings
            document.getElementById('nonceSearchMode').value = config.nonceSearchMode || 'sequential';
            document.getElementById('nonceStartMode').value = config.nonceStartMode || 'calculated';
            document.getElementById('useStride').checked = config.useStride || false;
            // First run: no config file yet — show configuration modal and default to testnet
            if (config.firstRun) {
                document.getElementById('configModal').style.display = 'block';
            }
        })
        .catch(error => {
            console.error('Error loading config:', error);
        });
}

function toggleRpcFieldsRequired(mode) {
    const required = mode === 'rpc';
    ['rpcUrl', 'rpcUser', 'rpcPass'].forEach(id => {
        const el = document.getElementById(id);
        if (el) {
            el.required = required;
            const parent = el.parentElement;
            if (parent) parent.classList.toggle('rpc-optional', !required);
        }
    });
}

function toggleRpcFieldsVisibility(mode) {
    const wrap = document.getElementById('rpcFieldsWrap');
    if (wrap) wrap.style.display = mode === 'rpc' ? 'block' : 'none';
}

function toggleTestConnectionButton(mode) {
    const btn = document.getElementById('testConnectionBtn');
    if (!btn) return;
    if (mode === 'p2p') {
        btn.disabled = true;
        btn.title = 'Not used in P2P mode — no node to test';
    } else {
        btn.disabled = false;
        btn.title = '';
    }
}

function toggleP2POptionsVisibility(mode) {
    const wrap = document.getElementById('p2pOptionsWrap');
    if (wrap) wrap.style.display = mode === 'p2p' ? 'block' : 'none';
}

function updateRpcUrlPlaceholder(network) {
    const rpcUrlEl = document.getElementById('rpcUrl');
    if (rpcUrlEl) rpcUrlEl.placeholder = network === 'testnet' ? 'http://localhost:44555' : 'http://localhost:22555';
}

function updateNetworkDependentFields(network) {
    const isTestnet = network === 'testnet';
    const payoutLabel = document.querySelector('#payoutAddrWrap label');
    const payoutInput = document.getElementById('payoutAddr');
    const payoutHint = document.getElementById('payoutAddrHint');
    if (payoutLabel) payoutLabel.textContent = isTestnet ? 'Payout Address (testnet):' : 'Payout Address (mainnet):';
    if (payoutInput) payoutInput.placeholder = isTestnet ? 'n...' : 'D...';
    if (payoutHint) payoutHint.textContent = isTestnet
        ? 'Testnet: use an n... address (required to run).'
        : 'Mainnet: use a D... address to receive block rewards (required).';
    const mainnetWrap = document.getElementById('p2pCheckpointMainnetWrap');
    const testnetWrap = document.getElementById('p2pCheckpointTestnetWrap');
    if (mainnetWrap) mainnetWrap.style.display = isTestnet ? 'none' : 'block';
    if (testnetWrap) testnetWrap.style.display = isTestnet ? 'block' : 'none';
    
    // Show/hide network-specific peer override fields
    const peerMainnetWrap = document.getElementById('p2pPeerOverrideMainnetWrap');
    const peerTestnetWrap = document.getElementById('p2pPeerOverrideTestnetWrap');
    if (peerMainnetWrap) peerMainnetWrap.style.display = isTestnet ? 'none' : 'block';
    if (peerTestnetWrap) peerTestnetWrap.style.display = isTestnet ? 'block' : 'none';
}

// Save current input to the stored slot for the given network (default: use dropdown value).
function savePayoutToStored(network) {
    if (network === undefined) network = (document.getElementById('network') && document.getElementById('network').value) || 'mainnet';
    const input = document.getElementById('payoutAddr');
    if (!input) return;
    const val = input.value.trim();
    if (network === 'testnet') storedPayoutTestnet = val;
    else storedPayoutMainnet = val;
}

function loadPayoutFromStored(network) {
    const input = document.getElementById('payoutAddr');
    if (!input) return;
    input.value = network === 'testnet' ? storedPayoutTestnet : storedPayoutMainnet;
}

function saveConfig() {
    const connectionMode = (document.getElementById('connectionMode') && document.getElementById('connectionMode').value) || 'rpc';
    const network = (document.getElementById('network') && document.getElementById('network').value) || 'mainnet';
    savePayoutToStored();
    const config = {
        network: network,
        connectionMode: connectionMode,
        rpcUrl: document.getElementById('rpcUrl').value.trim(),
        rpcUser: document.getElementById('rpcUser').value.trim(),
        rpcPass: document.getElementById('rpcPass').value.trim(),
        payoutAddr: network === 'testnet' ? storedPayoutTestnet : storedPayoutMainnet,
        payoutAddrMainnet: storedPayoutMainnet,
        payoutAddrTestnet: storedPayoutTestnet,
        p2pCheckpoint: (network === 'mainnet'
            ? (document.getElementById('p2pCheckpointMainnet') && document.getElementById('p2pCheckpointMainnet').value)
            : (document.getElementById('p2pCheckpointTestnet') && document.getElementById('p2pCheckpointTestnet').value)) || '',
        p2pPeerOverride: '', // Legacy field (deprecated)
        p2pPeerOverrideMainnet: (document.getElementById('p2pPeerOverrideMainnet') && document.getElementById('p2pPeerOverrideMainnet').value.trim()) || '',
        p2pPeerOverrideTestnet: (document.getElementById('p2pPeerOverrideTestnet') && document.getElementById('p2pPeerOverrideTestnet').value.trim()) || '',
        // Mining performance settings
        deviceType: document.getElementById('deviceType').value,
        threadCount: parseInt(document.getElementById('threadCount').value) || 1,
        miningIntensity: document.getElementById('miningIntensity').value,
        maxHashes: parseInt(document.getElementById('maxHashes').value) || 100000,
        // Nonce search strategy settings
        nonceSearchMode: document.getElementById('nonceSearchMode').value || 'sequential',
        nonceStartMode: document.getElementById('nonceStartMode').value || 'calculated',
        useStride: document.getElementById('useStride').checked || false
    };

    const statusDiv = document.getElementById('configStatus');
    const saveBtn = document.getElementById('saveConfigBtn');
    const saveBtnText = document.getElementById('saveConfigBtnText');

    // When using RPC, require RPC fields
    if (connectionMode === 'rpc' && (!config.rpcUrl || !config.rpcUser || !config.rpcPass)) {
        statusDiv.className = 'config-status error';
        statusDiv.textContent = 'Please fill in RPC URL, Username and Password when using RPC mode.';
        statusDiv.style.display = 'block';
        return;
    }
    // Payout address is required for mainnet and testnet
    const payoutVal = (network === 'testnet' ? storedPayoutTestnet : storedPayoutMainnet).trim();
    if (!payoutVal) {
        statusDiv.className = 'config-status error';
        statusDiv.textContent = 'Payout address is required for mainnet and testnet.';
        statusDiv.style.display = 'block';
        return;
    }

    if (saveBtn) {
        saveBtn.disabled = true;
        if (saveBtnText) saveBtnText.textContent = 'Saving…';
        saveBtn.classList.add('btn-saving');
    }

    fetch('/api/config', {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json'
        },
        body: JSON.stringify(config)
    })
    .then(response => response.json())
    .then(data => {
        if (saveBtn) {
            saveBtn.disabled = false;
            if (saveBtnText) saveBtnText.textContent = 'Save';
            saveBtn.classList.remove('btn-saving');
        }
        if (data.status === 'success') {
            statusDiv.className = 'config-status success';
            const message = data.restarted === 'true' 
                ? 'Configuration saved! Miner restarting with new settings...'
                : 'Configuration saved successfully!';
            statusDiv.textContent = message;
            statusDiv.style.display = 'block';
            setTimeout(() => {
                statusDiv.style.display = 'none';
                closeConfigModal();
            }, 2000);
            const logMessage = data.restarted === 'true'
                ? '✅ Configuration saved, miner restarting...'
                : '✅ Configuration saved successfully';
            addLogEntry(logMessage, 'success');
        } else {
            throw new Error(data.message || 'Failed to save configuration');
        }
    })
    .catch(error => {
        if (saveBtn) {
            saveBtn.disabled = false;
            if (saveBtnText) saveBtnText.textContent = 'Save';
            saveBtn.classList.remove('btn-saving');
        }
        statusDiv.className = 'config-status error';
        statusDiv.textContent = 'Error: ' + error.message;
        statusDiv.style.display = 'block';
        addLogEntry('Configuration error: ' + error.message, 'error');
    });
}

// Update maxHashes when intensity changes; connection mode change
document.addEventListener('DOMContentLoaded', function() {
    const intensitySelect = document.getElementById('miningIntensity');
    const maxHashesInput = document.getElementById('maxHashes');
    
    if (intensitySelect && maxHashesInput) {
        intensitySelect.addEventListener('change', function() {
            const intensity = this.value;
            switch(intensity) {
                case 'low':
                    maxHashesInput.value = 50000;
                    break;
                case 'medium':
                    maxHashesInput.value = 100000;
                    break;
                case 'high':
                    maxHashesInput.value = 500000;
                    break;
                case 'extreme':
                    maxHashesInput.value = 1000000;
                    break;
            }
        });
    }

    const networkEl = document.getElementById('network');
    if (networkEl) {
        networkEl.addEventListener('change', function() {
            const newNetwork = this.value || 'mainnet';
            savePayoutToStored(currentConfigNetwork);
            currentConfigNetwork = newNetwork;
            updateRpcUrlPlaceholder(newNetwork);
            updateNetworkDependentFields(newNetwork);
            loadPayoutFromStored(newNetwork);
        });
    }
    const connectionModeEl = document.getElementById('connectionMode');
    if (connectionModeEl) {
        connectionModeEl.addEventListener('change', function() {
            const mode = this.value || 'rpc';
            toggleRpcFieldsRequired(mode);
            toggleRpcFieldsVisibility(mode);
            toggleTestConnectionButton(mode);
            toggleP2POptionsVisibility(mode);
        });
    }
});

function testConnection() {
    const connectionMode = (document.getElementById('connectionMode') && document.getElementById('connectionMode').value) || 'rpc';
    const statusDiv = document.getElementById('configStatus');

    if (connectionMode === 'p2p') {
        statusDiv.className = 'config-status';
        statusDiv.textContent = 'P2P mode: no node required. Save configuration to use P2P.';
        statusDiv.style.display = 'block';
        return;
    }

    const config = {
        rpcUrl: document.getElementById('rpcUrl').value.trim(),
        rpcUser: document.getElementById('rpcUser').value.trim(),
        rpcPass: document.getElementById('rpcPass').value.trim()
    };

    if (!config.rpcUrl || !config.rpcUser || !config.rpcPass) {
        statusDiv.className = 'config-status error';
        statusDiv.textContent = 'Please fill in all RPC fields';
        statusDiv.style.display = 'block';
        return;
    }

    statusDiv.className = 'config-status';
    statusDiv.textContent = 'Testing connection...';
    statusDiv.style.display = 'block';

    const testBtn = document.getElementById('testConnectionBtn');
    testBtn.disabled = true;
    testBtn.textContent = 'Testing...';

    fetch('/api/test-connection', {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json'
        },
        body: JSON.stringify(config)
    })
    .then(response => response.json())
    .then(data => {
        if (data.status === 'success') {
            statusDiv.className = 'config-status success';
            statusDiv.textContent = '✓ Connection successful!';
            addLogEntry('RPC connection test successful', 'success');
        } else {
            statusDiv.className = 'config-status error';
            statusDiv.textContent = '✗ Connection failed: ' + (data.message || 'Unknown error');
            addLogEntry('RPC connection test failed: ' + (data.message || 'Unknown error'), 'error');
        }
    })
    .catch(error => {
        statusDiv.className = 'config-status error';
        statusDiv.textContent = '✗ Error: ' + error.message;
        addLogEntry('Connection test error: ' + error.message, 'error');
    })
    .finally(() => {
        testBtn.disabled = false;
        testBtn.textContent = 'Test';
    });
}

function openConfigModal() {
    document.getElementById('configModal').style.display = 'block';
    loadConfig(); // Load config first
    // Then update fields based on loaded network
    const network = (document.getElementById('network') && document.getElementById('network').value) || 'mainnet';
    const connectionMode = (document.getElementById('connectionMode') && document.getElementById('connectionMode').value) || 'rpc';
    updateNetworkDependentFields(network);
    toggleP2POptionsVisibility(connectionMode); // Ensure P2P options visibility is correct
}

function closeConfigModal() {
    document.getElementById('configModal').style.display = 'none';
    document.getElementById('configStatus').style.display = 'none';
}

function closeGenerateAddressModal() {
    document.getElementById('generateAddressModal').style.display = 'none';
}

function openGenerateAddressModal(address, privateKeyWIF) {
    const addrEl = document.getElementById('generatedAddressDisplay');
    const keyEl = document.getElementById('generatedPrivateKeyDisplay');
    if (addrEl) addrEl.value = address || '';
    if (keyEl) keyEl.value = privateKeyWIF || '';
    document.getElementById('generateAddressModal').style.display = 'block';
}

function generateAddress() {
    const network = (document.getElementById('network') && document.getElementById('network').value) || 'testnet';
    const btn = document.getElementById('generateAddressBtn');
    if (btn) btn.disabled = true;
    fetch('/api/generate-address', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ network: network })
    })
        .then(function(res) {
            if (!res.ok) return res.text().then(function(t) { throw new Error(t || res.statusText); });
            return res.json();
        })
        .then(function(data) {
            openGenerateAddressModal(data.address, data.privateKeyWIF);
        })
        .catch(function(err) {
            alert('Failed to generate address: ' + (err.message || err));
        })
        .finally(function() {
            if (btn) btn.disabled = false;
        });
}

function useGeneratedAddress() {
    const addrEl = document.getElementById('generatedAddressDisplay');
    const address = addrEl && addrEl.value ? addrEl.value.trim() : '';
    if (!address) return;
    const network = (document.getElementById('network') && document.getElementById('network').value) || 'mainnet';
    const payoutInput = document.getElementById('payoutAddr');
    if (payoutInput) payoutInput.value = address;
    if (network === 'testnet') storedPayoutTestnet = address;
    else storedPayoutMainnet = address;
    closeGenerateAddressModal();
}

function copyToClipboard(text, buttonEl) {
    if (!text) return;
    navigator.clipboard.writeText(text).then(function() {
        if (buttonEl) {
            var orig = buttonEl.textContent;
            buttonEl.textContent = 'Copied!';
            setTimeout(function() { buttonEl.textContent = orig; }, 1500);
        }
    }).catch(function() { alert('Copy failed'); });
}

// Event listeners
document.getElementById('startBtn').addEventListener('click', startMining);
document.getElementById('stopBtn').addEventListener('click', stopMining);
document.getElementById('configBtn').addEventListener('click', openConfigModal);
document.getElementById('closeConfig').addEventListener('click', closeConfigModal);
document.getElementById('cancelConfigBtn').addEventListener('click', closeConfigModal);
document.getElementById('testConnectionBtn').addEventListener('click', testConnection);
document.getElementById('configForm').addEventListener('submit', function(e) {
    e.preventDefault();
    saveConfig();
});

var generateAddressBtn = document.getElementById('generateAddressBtn');
if (generateAddressBtn) generateAddressBtn.addEventListener('click', generateAddress);

var useGeneratedAddressBtn = document.getElementById('useGeneratedAddressBtn');
if (useGeneratedAddressBtn) useGeneratedAddressBtn.addEventListener('click', useGeneratedAddress);

var closeGenerateAddressModalEl = document.getElementById('closeGenerateAddressModal');
if (closeGenerateAddressModalEl) closeGenerateAddressModalEl.addEventListener('click', closeGenerateAddressModal);
var closeGenerateAddressModalBtn = document.getElementById('closeGenerateAddressModalBtn');
if (closeGenerateAddressModalBtn) closeGenerateAddressModalBtn.addEventListener('click', closeGenerateAddressModal);

var copyAddressBtn = document.getElementById('copyAddressBtn');
if (copyAddressBtn) copyAddressBtn.addEventListener('click', function() {
    var el = document.getElementById('generatedAddressDisplay');
    copyToClipboard(el && el.value, copyAddressBtn);
});
var copyPrivateKeyBtn = document.getElementById('copyPrivateKeyBtn');
if (copyPrivateKeyBtn) copyPrivateKeyBtn.addEventListener('click', function() {
    var el = document.getElementById('generatedPrivateKeyDisplay');
    copyToClipboard(el && el.value, copyPrivateKeyBtn);
});

// Close modal when clicking outside
window.addEventListener('click', function(event) {
    const modal = document.getElementById('configModal');
    if (event.target === modal) {
        closeConfigModal();
    }
    var keyModal = document.getElementById('generateAddressModal');
    if (keyModal && event.target === keyModal) {
        closeGenerateAddressModal();
    }
});

// Hash logging toggle
const hashLogToggle = document.getElementById('hashLogToggle');
if (hashLogToggle) {
    // Initialize checkbox state (checked by default in HTML)
    showHashAttempts = hashLogToggle.checked;
    
    hashLogToggle.addEventListener('change', function(e) {
        showHashAttempts = e.target.checked;
        // Send preference to server
        fetch('/api/hash-logging', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled: showHashAttempts })
        }).catch(err => console.error('Error setting hash logging:', err));
        // Refresh log display immediately
        fetch('/api/stats')
            .then(response => response.json())
            .then(stats => {
                if (stats.logMessages) {
                    updateMiningLog(stats.logMessages);
                }
            })
            .catch(err => console.error('Error refreshing log:', err));
    });
}

// Clear logs button
const clearLogsBtn = document.getElementById('clearLogsBtn');
if (clearLogsBtn) {
    clearLogsBtn.addEventListener('click', function() {
        const log = document.getElementById('log');
        if (!log) return;
        
        // Clear all log entries
        log.innerHTML = '';
        
        // Add confirmation message
        const entry = document.createElement('div');
        entry.className = 'log-entry info';
        entry.textContent = `[${new Date().toLocaleTimeString()}] 🧹 Logs cleared`;
        log.appendChild(entry);
        
        // Reset message counter to prevent re-adding old messages
        lastLogMessageCount = 0;
        
        console.log('Logs cleared by user');
    });
}

// Initialize: load config first so first-run modal can open with testnet default
loadConfig();
loadInitialStats();
connectWebSocket();

// Refresh stats periodically (fallback when WebSocket is not connected)
setInterval(() => {
    if (!ws || ws.readyState !== WebSocket.OPEN) {
        fetch('/api/stats')
            .then(response => response.json())
            .then(stats => updateStats(stats))
            .catch(error => console.error('Error refreshing stats:', error));
    }
}, 5000);

// Mining log: same as compiled app — poll /api/logs every 4s and replace content so UI always shows latest
function refreshMiningLog() {
    fetch('/api/logs')
        .then(response => response.json())
        .then(data => {
            const logEl = document.getElementById('log');
            if (!logEl || !data.lines) return;
            logEl.innerHTML = '';
            const lines = data.lines;
            if (lines.length === 0) {
                const entry = document.createElement('div');
                entry.className = 'log-entry';
                entry.textContent = 'Waiting for log output...';
                logEl.appendChild(entry);
                return;
            }
            lines.forEach(msg => {
                const entry = document.createElement('div');
                entry.className = 'log-entry';
                if (msg.includes('BLOCK FOUND') || msg.includes('✅')) entry.className += ' success';
                else if (msg.includes('❌') || msg.includes('Error')) entry.className += ' error';
                else if (msg.includes('⚡') || msg.includes('⛏️') || msg.includes('📊')) entry.className += ' info';
                entry.textContent = msg;
                logEl.appendChild(entry);
            });
            logEl.scrollTop = logEl.scrollHeight;
        })
        .catch(() => {});
}
setInterval(refreshMiningLog, 4000);
// Initial load
setTimeout(refreshMiningLog, 500);
