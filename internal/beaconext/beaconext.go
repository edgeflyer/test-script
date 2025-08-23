package beaconext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// -------------------- 公共：读取 RPC 地址 --------------------

func GetRPCURL() (string, error) {
	// 同时兼容常见写法
	if v := os.Getenv("RPC_URL"); v != "" {
		return v, nil
	}
	if v := os.Getenv("rpcurl"); v != "" {
		return v, nil
	}
	return "", errors.New("missing RPC endpoint: set RPC_URL or rpcurl in env")
}

// -------------------- 通用 JSON-RPC --------------------

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      int64       `json:"id"`
}
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// doRPC：最小依赖的 JSON-RPC 调用，只有标准库
func doRPC(ctx context.Context, endpoint, method string, params interface{}, id int64) (json.RawMessage, error) {
	body, _ := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rpc http: %w", err)
	}
	defer resp.Body.Close()

	bz, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("rpc status %d: %s", resp.StatusCode, string(bz))
	}

	var r rpcResponse
	if err := json.Unmarshal(bz, &r); err != nil {
		return nil, fmt.Errorf("decode rpc response: %w", err)
	}
	if r.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", r.Error.Code, r.Error.Message)
	}
	return r.Result, nil
}

// -------------------- 结构体（按需精简/扩展） --------------------

// 执行层块（eth_getBlockByNumber 返回的最小字段）
type EthBlock struct {
	Number string `json:"number"`
	Hash   string `json:"hash"`
}

// 注意：下面两个接口返回结构在不同实现中可能不一致，先用 RawMessage 承接。
// 如果你的节点返回明确结构（如 { "hash": "0x..." } 或完整对象），再按需定义 struct。

// Beacon 区块
type BeaconBlock struct {
	Raw json.RawMessage `json:"-"`
}

// Beacon 状态
type BeaconState struct {
	Raw json.RawMessage `json:"-"`
}

// -------------------- 核心函数（最少参数，方便复用） --------------------

// 1) 获取最新执行层块哈希
func GetLatestEth1BlockHash(ctx context.Context, rpc string) (string, error) {
	res, err := doRPC(ctx, rpc, "eth_getBlockByNumber", []interface{}{"latest", false}, 1)
	if err != nil {
		return "", err
	}
	var blk EthBlock
	if err := json.Unmarshal(res, &blk); err != nil {
		return "", fmt.Errorf("decode eth block: %w", err)
	}
	if blk.Hash == "" {
		return "", errors.New("empty eth1 block hash")
	}
	return blk.Hash, nil
}

// 2) 由执行层块哈希映射到 Beacon 块哈希
// 有的实现直接返回字符串 "0x..."，也有可能是 {"hash": "..."}；两种都兼容。
func MapEth1HashToBeaconBlockHash(ctx context.Context, rpc, eth1Hash string) (string, error) {
	res, err := doRPC(ctx, rpc, "consensusBeaconExt_get_beacon_block_hash_by_eth1_hash", []string{eth1Hash}, 2)
	if err != nil {
		return "", err
	}

	// 优先尝试直接字符串
	var asString string
	if err := json.Unmarshal(res, &asString); err == nil && asString != "" {
		return asString, nil
	}

	// 尝试对象包装
	var asObj struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(res, &asObj); err == nil && asObj.Hash != "" {
		return asObj.Hash, nil
	}

	return "", fmt.Errorf("unexpected mapping result: %s", string(res))
}

// 3) 通过 Beacon 块哈希拉取 Beacon 区块（原样返回）
func GetBeaconBlockByHash(ctx context.Context, rpc, beaconBlockHash string) (*BeaconBlock, error) {
	res, err := doRPC(ctx, rpc, "consensusBeaconExt_get_beacon_block_by_hash", []string{beaconBlockHash}, 3)
	if err != nil {
		return nil, err
	}
	return &BeaconBlock{Raw: res}, nil
}

// 4) 通过 Beacon 块哈希拉取 BeaconState（原样返回）
func GetBeaconStateByBeaconBlockHash(ctx context.Context, rpc, beaconBlockHash string) (*BeaconState, error) {
	res, err := doRPC(ctx, rpc, "consensusBeaconExt_get_beacon_state_by_beacon_block_hash", []string{beaconBlockHash}, 4)
	if err != nil {
		return nil, err
	}
	return &BeaconState{Raw: res}, nil
}

// -------------------- 便捷封装：一把梭（可选） --------------------

// ChainGetBeaconState：不传 eth1Hash 时自动取最新块；传了就直接映射。
// timeout 建议 5~15s；为零则默认 10s。
func ChainGetBeaconState(ctx context.Context, rpc string, eth1HashOptional string, timeout time.Duration) (*BeaconState, string, string, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	eth1Hash := eth1HashOptional
	var err error
	if eth1Hash == "" {
		eth1Hash, err = GetLatestEth1BlockHash(ctx, rpc)
		if err != nil {
			return nil, "", "", fmt.Errorf("get latest eth1 block: %w", err)
		}
	}

	beaconBlockHash, err := MapEth1HashToBeaconBlockHash(ctx, rpc, eth1Hash)
	if err != nil {
		return nil, eth1Hash, "", fmt.Errorf("map eth1->beacon: %w", err)
	}

	state, err := GetBeaconStateByBeaconBlockHash(ctx, rpc, beaconBlockHash)
	if err != nil {
		return nil, eth1Hash, beaconBlockHash, fmt.Errorf("get beacon state: %w", err)
	}

	return state, eth1Hash, beaconBlockHash, nil
}
