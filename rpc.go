package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type RPCClient struct {
	url      string
	user     string
	password string
	client   *http.Client
}

type RPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type RPCResponse struct {
	Result interface{} `json:"result"`
	Error  *RPCError   `json:"error"`
	ID     int         `json:"id"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type BlockTemplate struct {
	Version          int32  `json:"version"`
	PreviousBlockHash string `json:"previousblockhash"`
	Transactions     []interface{} `json:"transactions"` // Ignored - we only mine empty blocks
	CoinbaseValue    int64  `json:"coinbasevalue"`
	CoinbaseScript   []byte `json:"-"`
	CoinbaseAux      map[string]interface{} `json:"coinbaseaux"`
	Target           string `json:"target"`
	Bits             string `json:"bits"`
	Height           int64  `json:"height"`
	Difficulty       float64 `json:"difficulty"`
	Mintime          int64  `json:"mintime"`
	CurTime          int64  `json:"curtime"`
	NonceRange       string `json:"noncerange"`
}

func NewRPCClient(url, user, password string) *RPCClient {
	return &RPCClient{
		url:      url,
		user:     user,
		password: password,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *RPCClient) TestConnection() error {
	_, err := c.Call("getblockcount", []interface{}{})
	return err
}

func (c *RPCClient) Call(method string, params []interface{}) (*RPCResponse, error) {
	req := &RPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	}

	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %v", err)
	}

	httpReq, err := http.NewRequest("POST", c.url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	httpReq.SetBasicAuth(c.user, c.password)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("RPC error: status %d, body: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %v", err)
	}

	var rpcResp RPCResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %v", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("RPC error: %s (code: %d)", rpcResp.Error.Message, rpcResp.Error.Code)
	}

	return &rpcResp, nil
}

func (c *RPCClient) GetBlockTemplate() (*BlockTemplate, error) {
	// Request block template with minimal data - we only need header info for empty block mining
	// We explicitly ignore transactions to mine empty blocks only
	params := []interface{}{map[string]interface{}{
		"mode": "template",
		"capabilities": []string{"proposal"}, // Request proposal mode (minimal data)
	}}

	resp, err := c.Call("getblocktemplate", params)
	if err != nil {
		return nil, err
	}

	// Convert result to JSON and back to parse properly
	resultJSON, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %v", err)
	}

	var template BlockTemplate
	if err := json.Unmarshal(resultJSON, &template); err != nil {
		return nil, fmt.Errorf("failed to unmarshal template: %v", err)
	}

	// Parse coinbase script from coinbaseaux if available
	if resultMap, ok := resp.Result.(map[string]interface{}); ok {
		if coinbaseAux, ok := resultMap["coinbaseaux"].(map[string]interface{}); ok {
			template.CoinbaseAux = coinbaseAux
			if flags, ok := coinbaseAux["flags"].(string); ok {
				flagsBytes, err := hex.DecodeString(flags)
				if err == nil {
					template.CoinbaseScript = flagsBytes
				}
			}
		}
		// If no coinbaseaux, create a simple script
		if len(template.CoinbaseScript) == 0 {
			template.CoinbaseScript = []byte{0x00} // Simple script
		}
	}

	return &template, nil
}

func (c *RPCClient) SubmitBlock(blockHex string) error {
	resp, err := c.Call("submitblock", []interface{}{blockHex})
	if err != nil {
		return fmt.Errorf("RPC call failed: %v", err)
	}
	
	// Check if result indicates rejection
	if resultStr, ok := resp.Result.(string); ok && resultStr != "" {
		return fmt.Errorf("block submission rejected: %s", resultStr)
	}
	
	return nil
}

func (c *RPCClient) GetBlockCount() (int64, error) {
	resp, err := c.Call("getblockcount", []interface{}{})
	if err != nil {
		return 0, err
	}

	count, ok := resp.Result.(float64)
	if !ok {
		return 0, fmt.Errorf("unexpected result type")
	}

	return int64(count), nil
}

func (c *RPCClient) GetBlockHash(height int64) (string, error) {
	resp, err := c.Call("getblockhash", []interface{}{height})
	if err != nil {
		return "", err
	}
	hash, ok := resp.Result.(string)
	if !ok {
		return "", fmt.Errorf("unexpected block hash type")
	}
	return hash, nil
}

// GetTipHeader returns the current chain tip as a BlockHeader by using getblockcount + getblockhash + getblockheader.
// The app can then build the block template locally via BuildTemplateFromHeader (no getblocktemplate needed).
func (c *RPCClient) GetTipHeader() (height int64, header *BlockHeader, err error) {
	height, err = c.GetBlockCount()
	if err != nil {
		return 0, nil, err
	}
	hash, err := c.GetBlockHash(height)
	if err != nil {
		return 0, nil, err
	}
	header, err = ParseBlockHeaderFromRPC(c, hash, height)
	if err != nil {
		return 0, nil, err
	}
	return height, header, nil
}

func (c *RPCClient) GetDifficulty() (float64, error) {
	resp, err := c.Call("getdifficulty", []interface{}{})
	if err != nil {
		return 0, err
	}

	diff, ok := resp.Result.(float64)
	if !ok {
		return 0, fmt.Errorf("unexpected result type")
	}

	return diff, nil
}
