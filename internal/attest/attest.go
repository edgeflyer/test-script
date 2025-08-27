package attest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	// 以太坊
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethrpc "github.com/ethereum/go-ethereum/rpc"

	// herumi BLS（避免 blst 汇编/CFI 问题）
	bls "github.com/herumi/bls-eth-go-binary/bls"
)

// -----------------------------
// 数据结构
// -----------------------------

// 与 Rust 侧一致
type AttestationData struct {
	Slot           uint64      `json:"slot"`
	CommitteeIndex uint64      `json:"committee_index"`
	ReceiptsRoot   common.Hash `json:"receipts_root"`
}

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      uint64      `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}
type jsonRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      uint64           `json:"id"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// -----------------------------
// BLS 初始化（ETH 模式）
// -----------------------------

func init() {
	if err := bls.Init(bls.BLS12_381); err != nil {
		panic(err)
	}
	// 以太坊模式；若服务端要求别的模式可调整
	bls.SetETHmode(bls.EthModeDraft07)
}

// -----------------------------
// 1) 计算 Receipts Root
// -----------------------------

// 通过执行层 RPC 拉取 receipts → 计算 receipts root
func ComputeReceiptsRootByHash(ctx context.Context, rpcURL string, blockHash common.Hash) (common.Hash, []*types.Receipt, error) {
	rpcCli, err := gethrpc.DialContext(ctx, rpcURL)
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("rpc dial: %w", err)
	}
	defer rpcCli.Close()

	// 1) 获取区块（含 tx hashes）
	var block map[string]interface{}
	if err := rpcCli.CallContext(ctx, &block, "eth_getBlockByHash", blockHash, false); err != nil {
		return common.Hash{}, nil, fmt.Errorf("eth_getBlockByHash: %w", err)
	}

	txs, ok := block["transactions"].([]interface{})
	if !ok {
		return common.Hash{}, nil, fmt.Errorf("unexpected block.transactions type")
	}

	// 2) 逐笔 receipt（兼容无 eth_getBlockReceipts 的节点）
	receipts := make([]*types.Receipt, 0, len(txs))
	for _, it := range txs {
		txHashHex, _ := it.(string)
		if !strings.HasPrefix(txHashHex, "0x") {
			return common.Hash{}, nil, fmt.Errorf("bad tx hash: %v", it)
		}
		var raw map[string]interface{}
		if err := rpcCli.CallContext(ctx, &raw, "eth_getTransactionReceipt", common.HexToHash(txHashHex)); err != nil {
			return common.Hash{}, nil, fmt.Errorf("eth_getTransactionReceipt %s: %w", txHashHex, err)
		}
		rcpt, err := decodeGethReceiptFromRPC(raw)
		if err != nil {
			return common.Hash{}, nil, fmt.Errorf("decode receipt: %w", err)
		}
		receipts = append(receipts, rcpt)
	}

	// 3) go-ethereum 计算 receipts root（Trie of receipts RLP）
	root := types.DeriveSha(types.Receipts(receipts), nil)
	return root, receipts, nil
}

// 按块号计算（内部转 hash）
func ComputeReceiptsRootByNumber(ctx context.Context, rpcURL string, number *big.Int) (common.Hash, []*types.Receipt, error) {
	rpcCli, err := gethrpc.DialContext(ctx, rpcURL)
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("rpc dial: %w", err)
	}
	defer rpcCli.Close()

	var block map[string]interface{}
	numHex := "latest"
	if number != nil {
		numHex = "0x" + strings.TrimLeft(strings.ToLower(number.Text(16)), "0")
		if numHex == "0x" {
			numHex = "0x0"
		}
	}
	if err := rpcCli.CallContext(ctx, &block, "eth_getBlockByNumber", numHex, false); err != nil {
		return common.Hash{}, nil, fmt.Errorf("eth_getBlockByNumber: %w", err)
	}
	hashStr, _ := block["hash"].(string)
	if !strings.HasPrefix(hashStr, "0x") {
		return common.Hash{}, nil, fmt.Errorf("block has no hash")
	}
	return ComputeReceiptsRootByHash(ctx, rpcURL, common.HexToHash(hashStr))
}

// 将 RPC JSON 映射为 *types.Receipt（最小字段，足够 DeriveSha）
func decodeGethReceiptFromRPC(raw map[string]interface{}) (*types.Receipt, error) {
	getHex := func(key string) (string, bool) { v, ok := raw[key].(string); return v, ok }
	statusHex, _ := getHex("status")
	cumulativeHex, _ := getHex("cumulativeGasUsed")
	logsBloomHex, _ := getHex("logsBloom")

	// status
	var status uint64 = 1
	if strings.HasPrefix(statusHex, "0x") {
		if x, ok := new(big.Int).SetString(statusHex[2:], 16); ok {
			status = x.Uint64()
		}
	}
	// cumulativeGasUsed
	var cumGas uint64
	if strings.HasPrefix(cumulativeHex, "0x") {
		if x, ok := new(big.Int).SetString(cumulativeHex[2:], 16); ok {
			cumGas = x.Uint64()
		}
	}

	// logs
	var logs []*types.Log
	if ls, ok := raw["logs"].([]interface{}); ok {
		logs = make([]*types.Log, 0, len(ls))
		for _, l := range ls {
			m, _ := l.(map[string]interface{})
			log := &types.Log{}
			if addr, ok := m["address"].(string); ok {
				log.Address = common.HexToAddress(addr)
			}
			if ts, ok := m["topics"].([]interface{}); ok {
				for _, t := range ts {
					if s, _ := t.(string); strings.HasPrefix(s, "0x") {
						log.Topics = append(log.Topics, common.HexToHash(s))
					}
				}
			}
			if data, ok := m["data"].(string); ok && strings.HasPrefix(data, "0x") {
				b, _ := hex.DecodeString(data[2:])
				log.Data = b
			}
			logs = append(logs, log)
		}
	}

	rcpt := &types.Receipt{
		Status:            status,
		CumulativeGasUsed: cumGas,
		Logs:              logs,
		TxHash:            common.HexToHash(safeString(raw["transactionHash"])),
		ContractAddress:   common.HexToAddress(safeString(raw["contractAddress"])),
		BlockHash:         common.HexToHash(safeString(raw["blockHash"])),
	}

	// logsBloom
	if strings.HasPrefix(logsBloomHex, "0x") {
		b, _ := hex.DecodeString(logsBloomHex[2:])
		if len(b) == types.BloomByteLength {
			var bloom types.Bloom
			copy(bloom[:], b)
			rcpt.Bloom = bloom
		}
	}
	return rcpt, nil
}

func safeString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// -----------------------------
// 2) 构造 + 签名 Attestation（含端序修正）
// -----------------------------

func BuildAttestationData(slot, committeeIdx uint64, receiptsRoot common.Hash) AttestationData {
	return AttestationData{Slot: slot, CommitteeIndex: committeeIdx, ReceiptsRoot: receiptsRoot}
}

// 保证与 Rust serde_json::to_vec 输出等价（无空格、键顺序固定）
func MarshalAttestationJSON(att AttestationData) ([]byte, error) {
	// receipts_root 必须是 0x + 64位小写hex
	root := strings.ToLower(att.ReceiptsRoot.Hex())
	// slot/committee_index 用十进制数字，不加引号
	s := fmt.Sprintf(`{"slot":%d,"committee_index":%d,"receipts_root":"%s"}`,
		att.Slot, att.CommitteeIndex, root,
	)
	return []byte(s), nil
}

// 端序工具：Rust/blst 导出 32B 为 big-endian；herumi 要求 little-endian
func reverseInPlace(b []byte) {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
}

// 从“Rust/blst 的 32B 大端 hex 私钥”加载 herumi SecretKey，并返回公钥（48B 压缩）
func loadHerumiSKFromRustBigEndianHex(skHex string) (*bls.SecretKey, []byte, error) {
	s := strings.TrimPrefix(skHex, "0x")
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, nil, fmt.Errorf("bad BLS SK hex (need 32B): %v", err)
	}
	reverseInPlace(b) // 大端 -> 小端
	var sec bls.SecretKey
	if err := sec.SetLittleEndian(b); err != nil {
		return nil, nil, fmt.Errorf("herumi SetLittleEndian: %w", err)
	}
	pk := sec.GetPublicKey().Serialize() // 48B 压缩 G1
	return &sec, pk, nil
}

// 仅派生公钥（用于订阅参数等）
func BLSPubKeyHex(skHex string) (string, error) {
	_, pk, err := loadHerumiSKFromRustBigEndianHex(skHex)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(pk), nil
}

// 签名（返回 96B 压缩 G2、48B 公钥）
func BLSSign(skHex string, msg []byte) (sig []byte, pk []byte, err error) {
	sec, pkBytes, err := loadHerumiSKFromRustBigEndianHex(skHex)
	if err != nil {
		return nil, nil, err
	}
	s := sec.SignByte(msg)
	return s.Serialize(), pkBytes, nil
}

// -----------------------------
// 3) 提交验证结果（HTTP JSON-RPC）
// -----------------------------

func SubmitVerification(ctx context.Context, rpcURL string, pubkeyHex, sigHex string, att AttestationData, recoveredBlockHash common.Hash) error {
	params := []interface{}{
		strings.TrimPrefix(pubkeyHex, "0x"),
		strings.TrimPrefix(sigHex, "0x"),
		att,
		strings.TrimPrefix(recoveredBlockHash.Hex(), "0x"),
	}
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "consensusBeaconExt_submitVerification",
		Params:  params,
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(&req); err != nil {
		return err
	}

	httpCli := &http.Client{Timeout: 10 * time.Second}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, &buf)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpCli.Do(httpReq)
	if err != nil {
		return fmt.Errorf("rpc http do: %w", err)
	}
	defer resp.Body.Close()

	var out jsonRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("rpc decode: %w", err)
	}
	if out.Error != nil {
		return fmt.Errorf("rpc error %d: %s", out.Error.Code, out.Error.Message)
	}
	// 如需查看 result，可解开注释：
	// if out.Result != nil { fmt.Printf("submit result: %s\n", string(*out.Result)) }
	return nil
}

// -----------------------------
// 4) 测试辅助：随机 BLS 私钥（32B hex）
// -----------------------------

func GenerateRandomBLSKey() (string, error) {
	ikm := make([]byte, 32)
	if _, err := rand.Read(ikm); err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(ikm), nil
}

// 传入：内层 header JSON（你的 payload 是 blockbody.header.header），以及我们算好的 receiptsRoot
func RecoveredBlockHashFromHeaderJSON(raw json.RawMessage, receiptsRoot common.Hash) (common.Hash, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return common.Hash{}, fmt.Errorf("decode header json: %w", err)
	}
	h := new(types.Header)
	getH := func(k string) (string, bool) { v, ok := m[k].(string); return v, ok }
	hexToHash := func(s string) common.Hash {
		if strings.HasPrefix(s, "0x") && len(s) == 66 {
			return common.HexToHash(s)
		}
		return common.Hash{}
	}
	hexToAddr := func(s string) common.Address {
		if strings.HasPrefix(s, "0x") && len(s) == 42 {
			return common.HexToAddress(s)
		}
		return common.Address{}
	}
	hexToBig := func(s string) *big.Int {
		if strings.HasPrefix(s, "0x") {
			if x, ok := new(big.Int).SetString(s[2:], 16); ok {
				return x
			}
			return nil
		}
		if s == "" {
			return nil
		}
		if x, ok := new(big.Int).SetString(s, 10); ok {
			return x
		}
		return nil
	}

	if v, ok := getH("parentHash"); ok {
		h.ParentHash = hexToHash(v)
	}
	if v, ok := getH("sha3Uncles"); ok {
		h.UncleHash = hexToHash(v)
	}
	if v, ok := getH("miner"); ok {
		h.Coinbase = hexToAddr(v)
	}
	if v, ok := getH("stateRoot"); ok {
		h.Root = hexToHash(v)
	}
	if v, ok := getH("transactionsRoot"); ok {
		h.TxHash = hexToHash(v)
	}

	// ⬇️ 用我们计算的 receiptsRoot 覆盖
	h.ReceiptHash = receiptsRoot

	if v, ok := getH("logsBloom"); ok && strings.HasPrefix(v, "0x") {
		b, _ := hex.DecodeString(v[2:])
		if len(b) == types.BloomByteLength {
			copy(h.Bloom[:], b)
		}
	}
	if v, ok := getH("difficulty"); ok {
		h.Difficulty = hexToBig(v)
	}
	if v, ok := getH("number"); ok {
		h.Number = hexToBig(v)
	}
	if v, ok := getH("gasLimit"); ok {
		if x := hexToBig(v); x != nil {
			h.GasLimit = x.Uint64()
		}
	}
	if v, ok := getH("gasUsed"); ok {
		if x := hexToBig(v); x != nil {
			h.GasUsed = x.Uint64()
		}
	}
	if v, ok := getH("timestamp"); ok {
		if x := hexToBig(v); x != nil {
			h.Time = x.Uint64()
		}
	}
	if v, ok := getH("extraData"); ok && strings.HasPrefix(v, "0x") {
		h.Extra, _ = hex.DecodeString(v[2:])
	}
	if v, ok := getH("mixHash"); ok {
		h.MixDigest = hexToHash(v)
	}
	if v, ok := getH("nonce"); ok && strings.HasPrefix(v, "0x") {
		var n types.BlockNonce
		if bb, _ := hex.DecodeString(v[2:]); len(bb) > 0 {
			copy(n[:], bb[len(bb)-8:])
			h.Nonce = n
		}
	}
	if v, ok := getH("baseFeePerGas"); ok {
		h.BaseFee = hexToBig(v)
	}
	if v, ok := getH("withdrawalsRoot"); ok {
		w := hexToHash(v)
		h.WithdrawalsHash = &w
	}
	if v, ok := getH("blobGasUsed"); ok {
		if x := hexToBig(v); x != nil {
			h.BlobGasUsed = x.Uint64()
		}
	}
	if v, ok := getH("excessBlobGas"); ok {
		if x := hexToBig(v); x != nil {
			h.ExcessBlobGas = x.Uint64()
		}
	}
	if v, ok := getH("parentBeaconBlockRoot"); ok {
		p := hexToHash(v)
		h.ParentBeaconRoot = &p
	}
	if v, ok := getH("requestsHash"); ok {
		r := hexToHash(v)
		h.RequestsHash = &r
	}

	return h.Hash(), nil
}
