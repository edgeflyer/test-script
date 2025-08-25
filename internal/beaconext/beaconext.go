package beaconext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// -------------------- 基础 JSON-RPC 客户端 --------------------

type Client struct {
	endpoint   string
	httpClient *http.Client
	idCounter  int64
}

func NewClient(endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      int64       `json:"id"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) call(ctx context.Context, method string, params interface{}, result any) error {
	id := atomic.AddInt64(&c.idCounter, 1)
	reqObj := rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	}
	body, err := json.Marshal(reqObj)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("http status %d: %s", resp.StatusCode, string(raw))
	}

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return fmt.Errorf("decode rpc response: %w", err)
	}
	if rpcResp.Error != nil {
		return fmt.Errorf("rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if result == nil {
		return nil
	}
	if len(rpcResp.Result) == 0 || string(rpcResp.Result) == "null" {
		return errors.New("empty result")
	}
	if err := json.Unmarshal(rpcResp.Result, result); err != nil {
		// 提示原始返回，便于排查类型不匹配
		return fmt.Errorf("unmarshal result: %w; raw=%s", err, string(rpcResp.Result))
	}
	return nil
}

// -------------------- 1) eth_getBlockByNumber --------------------

// EthGetBlockByNumber 返回最常用的区块头字段（可按需扩展）。
// tag: "latest" | "earliest" | "pending" | "safe" | "finalized" | 0x十六进制高度
// fullTx: 是否返回完整交易对象（false = 只返回 tx hash 列表）
func (c *Client) EthGetBlockByNumber(ctx context.Context, tag string, fullTx bool) (*EthBlock, error) {
	var out EthBlock
	// RPC 期望 params: [tag, fullTx]
	if err := c.call(ctx, "eth_getBlockByNumber", []any{tag, fullTx}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EthBlock 最小结构体，尽量通用；需要更多字段可自行添加 tag。
type EthBlock struct {
	Number           string   `json:"number"`
	Hash             string   `json:"hash"`
	ParentHash       string   `json:"parentHash"`
	MixHash          string   `json:"mixHash,omitempty"` // PoA/PoW 可能出现
	Nonce            string   `json:"nonce,omitempty"`
	Sha3Uncles       string   `json:"sha3Uncles"`
	LogsBloom        string   `json:"logsBloom,omitempty"`
	TransactionsRoot string   `json:"transactionsRoot"`
	StateRoot        string   `json:"stateRoot"`
	ReceiptsRoot     string   `json:"receiptsRoot"`
	Miner            string   `json:"miner"`
	Difficulty       string   `json:"difficulty,omitempty"`
	TotalDifficulty  string   `json:"totalDifficulty,omitempty"`
	ExtraData        string   `json:"extraData,omitempty"`
	Size             string   `json:"size,omitempty"`
	GasLimit         string   `json:"gasLimit"`
	GasUsed          string   `json:"gasUsed"`
	Timestamp        string   `json:"timestamp"`
	Uncles           []string `json:"uncles"`
	// 当 fullTx=false 时，Transactions 为 tx hash 数组；为 true 时是交易对象数组。
	// 为了兼容，这里用 RawMessage，你可根据需要再解析。
	Transactions  json.RawMessage `json:"transactions"`
	BaseFeePerGas string          `json:"baseFeePerGas,omitempty"`
	// 可选：保留所有未知字段
	//  _extra map[string]any `json:"-"`
}

// -------------------- 2) consensusBeaconExt_get_beacon_block_hash_by_eth1_hash --------------------

// GetBeaconBlockHashByEth1Hash 通过执行层区块哈希（eth1 hash）查信标链区块哈希。
// 返回为 0x 开头的十六进制字符串。
func (c *Client) GetBeaconBlockHashByEth1Hash(ctx context.Context, eth1Hash string) (string, error) {
	var out string
	if err := c.call(ctx, "consensusBeaconExt_get_beacon_block_hash_by_eth1_hash", []any{eth1Hash}, &out); err != nil {
		return "", err
	}
	return out, nil
}

// -------------------- 3) consensusBeaconExt_get_beacon_block_by_hash --------------------

// GetBeaconBlockByHash 通过信标区块哈希获取信标区块对象。
// 不强加结构，原样返回 JSON，便于不同客户端/分叉字段兼容。
func (c *Client) GetBeaconBlockByHash(ctx context.Context, beaconBlockHash string) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.call(ctx, "consensusBeaconExt_get_beacon_block_by_hash", []any{beaconBlockHash}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// -------------------- 4) consensusBeaconExt_get_beacon_state_by_beacon_block_hash --------------------

// GetBeaconStateByBeaconBlockHash 通过信标区块哈希获取对应状态。
// 同样返回 RawMessage，按需自行定义结构体再反序列化。
func (c *Client) GetBeaconStateByBeaconBlockHash(ctx context.Context, beaconBlockHash string) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.call(ctx, "consensusBeaconExt_get_beacon_state_by_beacon_block_hash", []any{beaconBlockHash}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// -------------------- 组合：给定 eth1 区块哈希，取信标块 + 信标状态 --------------------

type BeaconSnapshot struct {
	// 透传：输入的执行层区块哈希（for 追踪用）
	Eth1Hash string `json:"eth1_hash"`

	// 由 eth1_hash 解析出的信标区块哈希
	BeaconBlockHash string `json:"beacon_block_hash"`

	// 信标区块原始 JSON（不同客户端/分叉字段可能不同，保留 RawMessage 以保证兼容）
	BeaconBlockRaw json.RawMessage `json:"beacon_block_raw"`

	// 信标状态原始 JSON
	BeaconStateRaw json.RawMessage `json:"beacon_state_raw"`
}

// ResolveBeaconByEth1Hash 输入执行层区块哈希（0x...），返回信标区块与信标状态。
// 使用：snap, err := c.ResolveBeaconByEth1Hash(ctx, eth1Hash)
func (c *Client) ResolveBeaconByEth1Hash(ctx context.Context, eth1Hash string) (*BeaconSnapshot, error) {
	// 1) 执行层哈希 -> 信标区块哈希
	beaconHash, err := c.GetBeaconBlockHashByEth1Hash(ctx, eth1Hash)
	if err != nil {
		return nil, fmt.Errorf("map eth1 hash -> beacon block hash: %w", err)
	}
	if beaconHash == "" || beaconHash == "0x" {
		return nil, fmt.Errorf("empty beacon block hash for eth1 hash %s", eth1Hash)
	}

	// 2) 信标区块
	blkRaw, err := c.GetBeaconBlockByHash(ctx, beaconHash)
	if err != nil {
		return nil, fmt.Errorf("get beacon block by hash: %w", err)
	}

	// 3) 信标状态
	stateRaw, err := c.GetBeaconStateByBeaconBlockHash(ctx, beaconHash)
	if err != nil {
		return nil, fmt.Errorf("get beacon state by beacon block hash: %w", err)
	}

	return &BeaconSnapshot{
		Eth1Hash:        eth1Hash,
		BeaconBlockHash: beaconHash,
		BeaconBlockRaw:  blkRaw,
		BeaconStateRaw:  stateRaw,
	}, nil
}

// PrettyPrintJSON 将 json.RawMessage 格式化输出到控制台
func PrettyPrintJSON(label string, raw json.RawMessage) {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		fmt.Printf("%s (raw): %s\n", label, string(raw)) // 回退直接打印
		return
	}
	fmt.Printf("%s:\n%s\n", label, pretty.String())
}
