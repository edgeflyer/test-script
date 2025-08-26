package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"n42-test/internal/exit"
)

type JsonItem struct {
	// 你现有 JSON 里的字段：
	DepositPrivateKey string `json:"deposit-private-key"`  // 发起交易的 EOA 私钥（必有）
	ValidatorPubkey   string `json:"validator-public-key"` // BLS 公钥(48B hex)
	// 其他字段不影响退出，可保留不使用
	WithdrawalPrivateKey string `json:"withdrawal-private-key,omitempty"`
	WithdrawalAddress    string `json:"withdrawal-address,omitempty"`
	ValidatorPrivateKey  string `json:"validator-private-key,omitempty"`

	// 可选：如果以后想单独给退出用的私钥，也兼容
	ExitPrivateKey   string `json:"exit-private-key,omitempty"`
	ExitAmountWeiStr string `json:"exit-amount-wei,omitempty"` // 可选：退出请求里的 amount(wei)，默认 0
}

type Task struct {
	Index int
	Item  JsonItem
}

type Result struct {
	Index int
	Hash  string
	Err   error
	Block uint64
}

func main() {
	// ---------- CLI flags ----------
	jsonPath := flag.String("json", "deposit-data.json", "JSON 文件路径（数组）")
	rpcURL := flag.String("rpc", "http://127.0.0.1:8545", "执行层 RPC")
	contractAddr := flag.String("contract", "", "Exit 合约地址 (0x..)")
	mode := flag.String("mode", "concurrent", "sequential|concurrent")
	workers := flag.Int("workers", 4, "并发度，仅在 concurrent 模式下生效")
	start := flag.Int("start", 0, "起始 index（从0开始）")
	limit := flag.Int("limit", -1, "最大处理条数（<0 表示到末尾）")
	wait := flag.Bool("wait", true, "是否等待交易上链（true 等待回执，false 只发不等）")
	flag.Parse()

	if *contractAddr == "" || !common.IsHexAddress(*contractAddr) {
		log.Fatalf("必须提供合法的 --contract 地址")
	}
	contract := common.HexToAddress(*contractAddr)

	// ---------- load JSON ----------
	items, err := readJson(*jsonPath)
	if err != nil {
		log.Fatalf("读取 JSON 失败: %v", err)
	}
	items = sliceRange(items, *start, *limit)
	if len(items) == 0 {
		log.Println("无可处理条目，退出。")
		return
	}
	log.Printf("载入 %d 条退出请求（start=%d, limit=%d）", len(items), *start, *limit)

	// ---------- 构造任务 ----------
	tasks := make([]Task, len(items))
	for i, it := range items {
		tasks[i] = Task{Index: i + *start, Item: it} // 输出里的 Index 体现原始行号
	}

	ctx := context.Background()

	switch strings.ToLower(*mode) {
	case "sequential":
		runSequential(ctx, *rpcURL, contract, tasks, *wait)
	case "concurrent":
		runConcurrent(ctx, *rpcURL, contract, tasks, *workers, *wait)
	default:
		log.Fatalf("未知 mode=%s（可选 sequential|concurrent）", *mode)
	}
}

// ---------------- runners ----------------

func runSequential(ctx context.Context, rpc string, contract common.Address, tasks []Task, wait bool) {
	ok, fail := 0, 0
	for _, t := range tasks {
		res := handleOne(ctx, rpc, contract, t, wait)
		printResult(res)
		if res.Err != nil {
			fail++
		} else {
			ok++
		}
	}
	log.Printf("顺序退出完成：成功 %d，失败 %d", ok, fail)
}

func runConcurrent(ctx context.Context, rpc string, contract common.Address, tasks []Task, workers int, wait bool) {
	if workers <= 0 {
		workers = 1
	}
	in := make(chan Task)
	out := make(chan Result)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range in {
				res := handleOne(ctx, rpc, contract, t, wait)
				out <- res
			}
		}()
	}
	go func() {
		for _, t := range tasks {
			in <- t
		}
		close(in)
	}()
	go func() {
		wg.Wait()
		close(out)
	}()

	ok, fail := 0, 0
	for res := range out {
		printResult(res)
		if res.Err != nil {
			fail++
		} else {
			ok++
		}
	}
	log.Printf("并发退出完成：成功 %d，失败 %d (workers=%d)", ok, fail, workers)
}

// ---------------- core ----------------

func handleOne(ctx context.Context, rpc string, contract common.Address, task Task, wait bool) Result {
	idx := task.Index
	it := task.Item

	// 1) 选择发起交易的 EOA 私钥：优先 exit-private-key，其次 deposit-private-key
	rawKey := firstNonEmpty(it.ExitPrivateKey, it.DepositPrivateKey)
	if strings.TrimSpace(rawKey) == "" {
		return Result{Index: idx, Err: fmt.Errorf("缺少私钥（exit-private-key 或 deposit-private-key）")}
	}
	k := strings.TrimPrefix(strings.TrimSpace(rawKey), "0x")
	if len(k) != 64 {
		return Result{Index: idx, Err: fmt.Errorf("privKey hex 长度=%d，期望64（32字节）", len(k))}
	}
	priv, err := crypto.HexToECDSA(k)
	if err != nil {
		return Result{Index: idx, Err: fmt.Errorf("privKey 解析失败: %w", err)}
	}

	// 2) 解析 48B BLS 公钥
	pubkey, err := hexToBytes(it.ValidatorPubkey, 48)
	if err != nil {
		return Result{Index: idx, Err: fmt.Errorf("validator-public-key 错误: %w", err)}
	}

	// 3) 退出请求里的 amount（Wei），默认 0
	amt := big.NewInt(0)
	if strings.TrimSpace(it.ExitAmountWeiStr) != "" {
		z := new(big.Int)
		if _, ok := z.SetString(strings.TrimSpace(it.ExitAmountWeiStr), 10); ok {
			if z.Sign() >= 0 {
				amt = z
			} else {
				return Result{Index: idx, Err: errors.New("exit-amount-wei 不可为负")}
			}
		} else {
			return Result{Index: idx, Err: errors.New("exit-amount-wei 解析失败")}
		}
	}

	// 4) 执行发送
	client, err := ethclient.Dial(rpc)
	if err != nil {
		return Result{Index: idx, Err: fmt.Errorf("RPC 连接失败: %w", err)}
	}
	defer client.Close()

	ctx2, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	tx, rcpt, err := exit.SendExitRequest(ctx2, client, priv, contract, pubkey, amt, wait)
	if err != nil {
		return Result{Index: idx, Err: err}
	}

	r := Result{Index: idx, Hash: tx.Hash().Hex()}
	if rcpt != nil && rcpt.BlockNumber != nil {
		r.Block = rcpt.BlockNumber.Uint64()
	}
	return r
}

// ---------------- utils ----------------

func readJson(path string) ([]JsonItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var arr []JsonItem
	if err := json.NewDecoder(f).Decode(&arr); err != nil {
		return nil, err
	}
	if len(arr) == 0 {
		return nil, errors.New("JSON 空数组")
	}
	return arr, nil
}

func sliceRange[T any](in []T, start, limit int) []T {
	if start < 0 {
		start = 0
	}
	if start >= len(in) {
		return []T{}
	}
	end := len(in)
	if limit >= 0 && start+limit < end {
		end = start + limit
	}
	return in[start:end]
}

func hexToBytes(s string, want int) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != want {
		return nil, fmt.Errorf("invalid length %d, want %d", len(b), want)
	}
	return b, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func printResult(r Result) {
	if r.Err != nil {
		log.Printf("[#%d] ❌ 失败: %v", r.Index, r.Err)
		return
	}
	if r.Block > 0 {
		log.Printf("[#%d] ✅ 成功: tx=%s block=%d", r.Index, r.Hash, r.Block)
	} else {
		log.Printf("[#%d] ✅ 已发送: tx=%s", r.Index, r.Hash)
	}
}
