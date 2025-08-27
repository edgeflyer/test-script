package attest

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/gorilla/websocket"
)

// --- JSON-RPC over WS ---

type wsRPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      uint64        `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params,omitempty"`
}
type wsRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      uint64           `json:"id,omitempty"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  *wsSubParams     `json:"params,omitempty"`
}
type wsSubParams struct {
	Subscription string           `json:"subscription"`
	Result       *json.RawMessage `json:"result"`
}

// ---- 统一解析：先拿到 blockbody.header 的原始 JSON，再分形态解码 ----

type unverifiedBlockPush struct {
	BlockBody struct {
		Header json.RawMessage `json:"header"` // 可能是 {header:{...}, body:{...}} 或直接 {number:...}
		Body   json.RawMessage `json:"body"`   // 用于取交易个数（为空块时可快速给出 empty trie）
	} `json:"blockbody"`
	CommitteeIndex uint64 `json:"committee_index"`
}

// 形态 A：blockbody.header = { "header": { "number": ... }, "body": {...} }
type sealedBlockEnvelopeA struct {
	Header headerMinimal   `json:"header"`
	Body   json.RawMessage `json:"body"`
}

// 形态 B：blockbody.header = { "number": ... }
type headerMinimal struct {
	Number *json.RawMessage `json:"number,omitempty"`
	Hash   *string          `json:"hash,omitempty"` // 有些实现会放 header.hash，这里也兼容
}

// body 最小结构：只关心交易个数
type bodyMinimal struct {
	Transactions []json.RawMessage `json:"transactions"`
}

// 把 RawMessage 里可能的 "0x..." / "12345" / 12345 解析为 uint64
func parseUint64JSON(r *json.RawMessage) (uint64, error) {
	if r == nil {
		return 0, fmt.Errorf("nil")
	}
	// 先按字符串试
	var s string
	if err := json.Unmarshal(*r, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, fmt.Errorf("empty string")
		}
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			n, ok := new(big.Int).SetString(s[2:], 16)
			if !ok {
				return 0, fmt.Errorf("bad hex number: %s", s)
			}
			return n.Uint64(), nil
		}
		u, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad dec number: %s", s)
		}
		return u, nil
	}
	// 再按无引号数值试
	var u64 uint64
	if err := json.Unmarshal(*r, &u64); err == nil {
		return u64, nil
	}
	// 再按 json.Number 试
	var num json.Number
	if err := json.Unmarshal(*r, &num); err == nil {
		if strings.HasPrefix(num.String(), "0x") {
			n, ok := new(big.Int).SetString(num.String()[2:], 16)
			if !ok {
				return 0, fmt.Errorf("bad hex number: %s", num.String())
			}
			return n.Uint64(), nil
		}
		u, err := strconv.ParseUint(num.String(), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad number: %s", num.String())
		}
		return u, nil
	}
	return 0, fmt.Errorf("unknown number encoding")
}

// --- Runner 配置 ---

type WSRunnerConfig struct {
	WSURL         string
	ExecRPCURL    string
	SubmitRPCURL  string
	BLSSecretHex  string
	PubkeyHexOut  func(pkHex string)
	Logf          func(format string, args ...any)
	RetryMax      int
	RetryInterval time.Duration
}

// --- 等待配置（可按需调大/调小） ---
const (
	waitBlockTimeout   = 20 * time.Second // 等待执行层出现该块的最长时间
	waitPollInterval   = 500 * time.Millisecond
	waitReceiptTimeout = 20 * time.Second // 等待 receipts 可读的最长时间
)

// RunWSValidator：连接 WS → 订阅 → 循环处理（失败自动重连）
func RunWSValidator(ctx context.Context, cfg WSRunnerConfig) error {
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 5
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = 5 * time.Second
	}

	// 从私钥派生 pk（端序已在 BLSPubKeyHex 内处理）
	pkHex, err := BLSPubKeyHex(cfg.BLSSecretHex)
	if err != nil {
		return fmt.Errorf("derive bls pk: %w", err)
	}
	if cfg.PubkeyHexOut != nil {
		cfg.PubkeyHexOut(pkHex)
	}

	retries := 0
	for {
		err := runOnce(ctx, cfg, pkHex, logf)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		retries++
		if retries > cfg.RetryMax {
			return fmt.Errorf("ws validator: max retries reached, last error: %w", err)
		}
		logf("ws validator: error: %v, retrying in %s (%d/%d)", err, cfg.RetryInterval, retries, cfg.RetryMax)
		select {
		case <-time.After(cfg.RetryInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func runOnce(ctx context.Context, cfg WSRunnerConfig, pkHex string, logf func(string, ...any)) error {
	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.DialContext(ctx, cfg.WSURL, nil)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.Close()

	logf("ws connected: %s", cfg.WSURL)

	// 1) subscribe（如服务端需要 0x 前缀，去掉 TrimPrefix）
	subReq := wsRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "consensusBeaconExt_subscribeToVerificationRequest",
		Params:  []interface{}{strings.TrimPrefix(pkHex, "0x")},
	}
	if err := conn.WriteJSON(subReq); err != nil {
		return fmt.Errorf("ws write subscribe: %w", err)
	}
	logf("sent subscribe: %s", subReq.Method)

	// 2) read subscribe resp
	var subResp wsRPCResponse
	if err := conn.ReadJSON(&subResp); err != nil {
		return fmt.Errorf("ws read subscribe resp: %w", err)
	}
	if subResp.Error != nil {
		return fmt.Errorf("ws subscribe rpc error %d: %s", subResp.Error.Code, subResp.Error.Message)
	}
	logf("subscribe ok: %s", bytesOrNull(subResp.Result))

	// 3) handle pushes
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var msg wsRPCResponse
		if err := conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("ws read: %w", err)
		}
		if msg.Params == nil || msg.Params.Result == nil {
			continue
		}

		raw := *msg.Params.Result

		// 解析外层
		var ub unverifiedBlockPush
		if err := json.Unmarshal(raw, &ub); err != nil {
			logf("bad UnverifiedBlock: %v, raw=%s", err, string(raw))
			continue
		}

		// 尝试两种 header 形态拿 number/hash
		var (
			slot uint64
			hash common.Hash
			ok   bool
		)

		// 形态 A：{ "header": { "number": ... }, "body": {...} }
		var envA sealedBlockEnvelopeA
		if err := json.Unmarshal(ub.BlockBody.Header, &envA); err == nil {
			if envA.Header.Number != nil {
				if v, err := parseUint64JSON(envA.Header.Number); err == nil {
					slot = v
					ok = true
				} else {
					logf("header.header.number parse fail: %v", err)
				}
			}
			// 如有 header.hash 也兼容
			if !ok && envA.Header.Hash != nil {
				h := *envA.Header.Hash
				if strings.HasPrefix(h, "0x") && len(h) == 66 {
					hash = common.HexToHash(h)
					ok = true
				} else {
					logf("header.header.hash invalid: %s", h)
				}
			}
		}

		// 形态 B：{ "number": ... }
		if !ok {
			var hdrB headerMinimal
			if err := json.Unmarshal(ub.BlockBody.Header, &hdrB); err == nil {
				if hdrB.Number != nil {
					if v, err := parseUint64JSON(hdrB.Number); err == nil {
						slot = v
						ok = true
					} else {
						logf("header.number parse fail: %v", err)
					}
				}
				if !ok && hdrB.Hash != nil {
					h := *hdrB.Hash
					if strings.HasPrefix(h, "0x") && len(h) == 66 {
						hash = common.HexToHash(h)
						ok = true
					} else {
						logf("header.hash invalid: %s", h)
					}
				}
			}
		}

		// 两者仍都没有，打印 payload 跳过
		if !ok {
			logf("insufficient fields (no number/hash), raw=%s", string(raw))
			continue
		}

		// 统计交易个数（为空块可用 empty trie 作为 receipts root 的快速路径）
		txCount := 0
		if len(ub.BlockBody.Body) > 0 {
			var bm bodyMinimal
			if err := json.Unmarshal(ub.BlockBody.Body, &bm); err == nil {
				txCount = len(bm.Transactions)
			}
		}

		// 计算 receipts_root + 确认最终的 hash 与 slot（加入重试等待）
		var (
			root common.Hash
			err2 error
		)

		if (hash != common.Hash{}) {
			// 已有哈希：等待该块在执行层可见，然后按哈希算 receipts root
			if h, errw := waitForBlockHashVisible(ctx, cfg.ExecRPCURL, hash, waitBlockTimeout, waitPollInterval, logf); errw != nil {
				logf("waitForBlockHashVisible err: %v", errw)
				continue
			} else {
				hash = h // 只是确认一下
			}
			root, err2 = computeReceiptsRootByHashWithRetry(ctx, cfg.ExecRPCURL, hash, waitReceiptTimeout, waitPollInterval, txCount, logf)
			if err2 != nil {
				logf("computeReceiptsRootByHashWithRetry err: %v", err2)
				continue
			}
			// 若还不知道 slot，就反查一下
			if slot == 0 {
				if s, err3 := getBlockNumberByHash(ctx, cfg.ExecRPCURL, hash); err3 == nil {
					slot = s
				}
			}
		} else {
			// 只有块号：等待该号的块出现，拿到哈希，再按哈希算 receipts root
			h, errw := waitForBlockHashByNumber(ctx, cfg.ExecRPCURL, slot, waitBlockTimeout, waitPollInterval, logf)
			if errw != nil {
				logf("waitForBlockHashByNumber err: %v", errw)
				continue
			}
			hash = h
			root, err2 = computeReceiptsRootByHashWithRetry(ctx, cfg.ExecRPCURL, hash, waitReceiptTimeout, waitPollInterval, txCount, logf)
			if err2 != nil {
				logf("computeReceiptsRootByHashWithRetry err: %v", err2)
				continue
			}
		}

		// 组装 & 签名
		att := BuildAttestationData(slot, ub.CommitteeIndex, root)
		msgBytes, _ := MarshalAttestationJSON(att)
		sig, pk, err := BLSSign(cfg.BLSSecretHex, msgBytes)
		if err != nil {
			logf("BLSSign err: %v", err)
			continue
		}
		pkHex2 := "0x" + hex.EncodeToString(pk)
		sigHex := "0x" + hex.EncodeToString(sig)

		// 提交（用确认的真实哈希）
		if err := SubmitVerification(ctx, cfg.SubmitRPCURL, pkHex2, sigHex, att, hash); err != nil {
			logf("SubmitVerification err: %v", err)
			continue
		}
		logf("attested: slot=%d committee=%d root=%s hash=%s", slot, ub.CommitteeIndex, root.Hex(), hash.Hex())
	}
}

func bytesOrNull(r *json.RawMessage) string {
	if r == nil {
		return "null"
	}
	return string(*r)
}

// ---------- HTTP JSON-RPC helpers ----------

// 轻量实现：按块号取 block hash（HTTP JSON-RPC）
func getBlockHashByNumber(ctx context.Context, rpcURL string, number uint64) (common.Hash, error) {
	type reqT struct {
		JSONRPC string        `json:"jsonrpc"`
		ID      int           `json:"id"`
		Method  string        `json:"method"`
		Params  []interface{} `json:"params"`
	}
	numHex := fmt.Sprintf("0x%x", number)
	body, _ := json.Marshal(reqT{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_getBlockByNumber",
		Params:  []interface{}{numHex, false},
	})
	httpCli := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "POST", rpcURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpCli.Do(req)
	if err != nil {
		return common.Hash{}, err
	}
	defer resp.Body.Close()

	var out struct {
		JSONRPC string           `json:"jsonrpc"`
		ID      int              `json:"id"`
		Result  *json.RawMessage `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return common.Hash{}, err
	}
	if out.Error != nil {
		return common.Hash{}, fmt.Errorf("rpc error %d: %s", out.Error.Code, out.Error.Message)
	}
	if out.Result == nil {
		return common.Hash{}, fmt.Errorf("empty result")
	}
	var block map[string]any
	if err := json.Unmarshal(*out.Result, &block); err != nil {
		return common.Hash{}, err
	}
	h, _ := block["hash"].(string)
	if !strings.HasPrefix(h, "0x") || len(h) != 66 {
		return common.Hash{}, fmt.Errorf("bad block hash: %v", h)
	}
	return common.HexToHash(h), nil
}

// 轻量实现：按块哈希取块号（HTTP JSON-RPC）
func getBlockNumberByHash(ctx context.Context, rpcURL string, hash common.Hash) (uint64, error) {
	type reqT struct {
		JSONRPC string        `json:"jsonrpc"`
		ID      int           `json:"id"`
		Method  string        `json:"method"`
		Params  []interface{} `json:"params"`
	}
	body, _ := json.Marshal(reqT{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_getBlockByHash",
		Params:  []interface{}{hash.Hex(), false},
	})
	httpCli := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "POST", rpcURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpCli.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var out struct {
		Result *struct {
			Number string `json:"number"` // 0x...
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	if out.Error != nil {
		return 0, fmt.Errorf("rpc error %d: %s", out.Error.Code, out.Error.Message)
	}
	if out.Result == nil || out.Result.Number == "" {
		return 0, fmt.Errorf("empty result.number")
	}
	n, ok := new(big.Int).SetString(strings.TrimPrefix(out.Result.Number, "0x"), 16)
	if !ok {
		return 0, fmt.Errorf("bad number: %s", out.Result.Number)
	}
	return n.Uint64(), nil
}

// ---------- Wait & Retry helpers ----------

// 等到“按块号能拿到哈希”为止
func waitForBlockHashByNumber(ctx context.Context, rpcURL string, number uint64, timeout, interval time.Duration, logf func(string, ...any)) (common.Hash, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		h, err := getBlockHashByNumber(ctx, rpcURL, number)
		if err == nil {
			return h, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return common.Hash{}, fmt.Errorf("waitForBlockHashByNumber timeout: %w", lastErr)
		}
		logf("waitForBlockHashByNumber: number=%d not ready yet (%v), retry...", number, err)
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return common.Hash{}, ctx.Err()
		}
	}
}

// 确认“按哈希查询能返回该块”（避免马上查 receipts 报 not found）
func waitForBlockHashVisible(ctx context.Context, rpcURL string, hash common.Hash, timeout, interval time.Duration, logf func(string, ...any)) (common.Hash, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if _, err := getBlockNumberByHash(ctx, rpcURL, hash); err == nil {
			return hash, nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return common.Hash{}, fmt.Errorf("waitForBlockHashVisible timeout: %w", lastErr)
		}
		logf("waitForBlockHashVisible: hash=%s not ready yet (%v), retry...", hash.Hex(), lastErr)
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return common.Hash{}, ctx.Err()
		}
	}
}

// 带重试地按哈希计算 receipts root；如果 txCount==0，直接返回空 Trie 的根
func computeReceiptsRootByHashWithRetry(ctx context.Context, rpcURL string, hash common.Hash, timeout, interval time.Duration, txCount int, logf func(string, ...any)) (common.Hash, error) {
	// 空块的 receiptsRoot 恒等于空 trie（和以太坊规范一致）
	if txCount == 0 {
		// 以太坊“空 Trie”常量：0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421
		return common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"), nil
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		root, _, err := ComputeReceiptsRootByHash(ctx, rpcURL, hash)
		if err == nil {
			return root, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return common.Hash{}, fmt.Errorf("computeReceiptsRootByHashWithRetry timeout: %w", lastErr)
		}
		logf("computeReceiptsRootByHashWithRetry: hash=%s not ready (%v), retry...", hash.Hex(), lastErr)
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return common.Hash{}, ctx.Err()
		}
	}
}
