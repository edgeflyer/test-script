package main

import (
	"context"
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

	// 改成你项目的真实模块路径
	"n42-test/internal/deposit"
)

type JsonItem struct {
	WithdrawalPrivateKey string `json:"withdrawal-private-key"` // 目前未用
	ValidatorPublicKey   string `json:"validator-public-key"`   // BLS 公钥(48B hex，无0x也可)
	WithdrawalAddress    string `json:"withdrawal-address"`     // 20B exec addr（0x…）
	ValidatorPrivateKey  string `json:"validator-private-key"`  // BLS 私钥(用于签名)
	DepositPrivateKey    string `json:"deposit-private-key"`    // 发交易的 EOA 私钥（secp256k1）
}

type Task struct {
	Index int
	Item  JsonItem
}

type Result struct {
	Index        int
	Hash         string
	Err          error
	Nonce        uint64
	UsedGas      uint64
	EstimatedGas uint64
	BlockNumber  uint64
	BlockHash    string
}

func main() {
	deposit.EnsureBLS()

	// ---------- CLI flags ----------
	jsonPath := flag.String("json", "accounts.json", "JSON 文件路径（数组）")
	rpcURL := flag.String("rpc", "http://127.0.0.1:8545", "执行层 RPC")
	contractAddr := flag.String("contract", "", "Deposit 合约地址（0x…）")
	mode := flag.String("mode", "concurrent", "发送模式：sequential|concurrent")
	workers := flag.Int("workers", 8, "并发度，仅在 --mode=concurrent 生效")
	orderedOut := flag.Bool("ordered-output", true, "并发模式下是否按输入顺序输出结果")
	start := flag.Int("start", 0, "从第几条（基于0）开始处理")
	limit := flag.Int("limit", -1, "最多处理多少条；<0 表示全部")
	dryRun := flag.Bool("dry-run", false, "仅打印将要发送的摘要，不真正上链")
	noWait := flag.Bool("no-wait", false, "不等待回执，发送后立即返回")

	amountETH := flag.Float64("amount-eth", 32, "每笔质押金额（ETH，默认32）。与 --amount-wei 互斥")
	amountWeiStr := flag.String("amount-wei", "", "每笔质押金额（Wei，字符串）。若设置则覆盖 --amount-eth")

	// 手动费用（留空则自动）
	gasLimit := flag.Uint64("gas-limit", 0, "GasLimit（0=自动估算）")
	maxTipGwei := flag.Float64("max-tip-gwei", 0, "MaxPriorityFeePerGas（单位 Gwei，0=自动建议）")
	maxFeeGwei := flag.Float64("max-fee-gwei", 0, "MaxFeePerGas（单位 Gwei，0=自动建议）")

	flag.Parse()

	if *contractAddr == "" || !common.IsHexAddress(*contractAddr) {
		log.Fatalf("必须提供合法的 --contract 合约地址 (0x...)")
	}
	if *noWait {
		log.Println("⚡ no-wait 模式：发送后不等待回执")
	}

	// ---------- 读取 JSON ----------
	items, err := readJson(*jsonPath)
	if err != nil {
		log.Fatalf("读取 JSON 失败: %v", err)
	}
	// 截取 start/limit
	items = sliceRange(items, *start, *limit)
	if len(items) == 0 {
		log.Println("无可处理条目，退出。")
		return
	}
	log.Printf("共载入 %d 条（start=%d, limit=%d）", len(items), *start, *limit)

	// ---------- 计算金额 ----------
	amountWei, err := decideAmount(*amountWeiStr, *amountETH)
	if err != nil {
		log.Fatalf("金额参数错误: %v", err)
	}

	// EIP-1559 手动费
	var maxTipWei, maxFeeWei *big.Int
	if *maxTipGwei > 0 {
		maxTipWei = gweiF(*maxTipGwei)
	}
	if *maxFeeGwei > 0 {
		maxFeeWei = gweiF(*maxFeeGwei)
	}

	// ---------- 构造任务 ----------
	tasks := make([]Task, len(items))
	for i, it := range items {
		tasks[i] = Task{Index: i, Item: it}
	}

	// ---------- 跑任务 ----------
	ctx := context.Background()

	switch strings.ToLower(*mode) {
	case "sequential":
		runSequential(ctx, *rpcURL, *contractAddr, tasks, amountWei, *gasLimit, maxTipWei, maxFeeWei, *dryRun, *noWait)
	case "concurrent":
		runConcurrent(ctx, *rpcURL, *contractAddr, tasks, *workers, amountWei, *gasLimit, maxTipWei, maxFeeWei, *dryRun, *orderedOut, *noWait)
	default:
		log.Fatalf("未知的 --mode：%s（可选 sequential|concurrent）", *mode)
	}
}

// ---------------- 任务执行 ----------------

func runSequential(
	ctx context.Context,
	rpc, contract string,
	tasks []Task,
	amountWei *big.Int,
	gasLimit uint64,
	maxTipWei, maxFeeWei *big.Int,
	dryRun bool,
	noWait bool,
) {
	ok, fail := 0, 0
	startAt := time.Now()

	for _, t := range tasks {
		res := handleOne(ctx, rpc, contract, t, amountWei, gasLimit, maxTipWei, maxFeeWei, dryRun, noWait)
		printResult(res)
		if res.Err != nil {
			fail++
		} else {
			ok++
		}
	}

	log.Printf("顺序完成：成功 %d，失败 %d，耗时 %s", ok, fail, time.Since(startAt).Round(time.Millisecond))
}

func runConcurrent(
	ctx context.Context,
	rpc, contract string,
	tasks []Task,
	workers int,
	amountWei *big.Int,
	gasLimit uint64,
	maxTipWei, maxFeeWei *big.Int,
	dryRun bool,
	orderedOutput bool,
	noWait bool,
) {
	if workers <= 0 {
		workers = 4
	}

	startAt := time.Now()
	in := make(chan Task)
	out := make(chan Result)

	var wg sync.WaitGroup
	// worker pool
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range in {
				res := handleOne(ctx, rpc, contract, t, amountWei, gasLimit, maxTipWei, maxFeeWei, dryRun, noWait)
				out <- res
			}
		}()
	}

	// 收集者
	go func() {
		wg.Wait()
		close(out)
	}()

	go func() {
		for _, t := range tasks {
			in <- t
		}
		close(in)
	}()

	ok, fail := 0, 0

	if !orderedOutput {
		// 到达即打
		for res := range out {
			printResult(res)
			if res.Err != nil {
				fail++
			} else {
				ok++
			}
		}
	} else {
		// 按输入顺序输出：用缓冲 map，维护 nextIndex
		buf := make(map[int]Result, len(tasks))
		next := 0
		for res := range out {
			buf[res.Index] = res
			for {
				if r, ok2 := buf[next]; ok2 {
					printResult(r)
					if r.Err != nil {
						fail++
					} else {
						ok++
					}
					delete(buf, next)
					next++
				} else {
					break
				}
			}
		}
	}

	log.Printf("并发完成：成功 %d，失败 %d，并发度 %d，耗时 %s", ok, fail, workers, time.Since(startAt).Round(time.Millisecond))
}

// 实际处理一条：构造 DepositParams 并发交易
func handleOne(
	ctx context.Context,
	rpc, contract string,
	task Task,
	amountWei *big.Int,
	gasLimit uint64,
	maxTipWei, maxFeeWei *big.Int,
	dryRun bool,
	noWait bool,
) Result {
	idx := task.Index
	it := task.Item

	// 1) 生成 WC
	wc, err := deposit.ComputeWithdrawalCredentialsFromEth1(it.WithdrawalAddress)
	if err != nil {
		return Result{Index: idx, Err: fmt.Errorf("index %d: 生成WC失败: %w", idx, err)}
	}

	// 2) 生成签名 + deposit_data_root
	//    将交易金额 Wei -> Gwei，用于 BLS 的 amount 字段
	amountGwei := new(big.Int).Div(new(big.Int).Set(amountWei), big.NewInt(1_000_000_000)).Uint64()

	sigHex, rootHex, err := deposit.ComputeDepositSignatureAndRoot(
		it.ValidatorPublicKey,
		wc,
		amountGwei, // 与交易金额对齐
		it.ValidatorPrivateKey,
	)
	if err != nil {
		return Result{Index: idx, Err: fmt.Errorf("index %d: 计算签名/根失败: %w", idx, err)}
	}

	// 3) 准备参数
	params := &deposit.DepositParams{
		Contract:             contract,
		PrivateKeyHex:        it.DepositPrivateKey,
		RPC:                  rpc,
		PubkeyHex:            it.ValidatorPublicKey,
		WCHex:                wc,
		SignatureHex:         sigHex,
		RootHex:              rootHex,
		AmountWei:            new(big.Int).Set(amountWei),
		Nonce:                -1, // 自动取 nonce
		GasLimit:             gasLimit,
		MaxPriorityFeePerGas: maxTipWei,
		MaxFeePerGas:         maxFeeWei,
	}

	if dryRun {
		return Result{
			Index: idx,
			Hash:  "(dry-run)",
			Err:   nil,
		}
	}

	// 4) 发送交易：使用每条目的私钥新建 client
	ctx2, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	cli, err := deposit.NewClient(ctx2, params.RPC, params.PrivateKeyHex)
	if err != nil {
		return Result{Index: idx, Err: fmt.Errorf("index %d: NewClient 失败: %w", idx, err)}
	}
	defer cli.Close()

	txRes, err := func() (*deposit.TxResult, error) {
		if noWait {
			return cli.SendDepositNoWait(ctx2, params)
		}
		return cli.SendDeposit(ctx2, params)
	}()
	if err != nil {
		return Result{Index: idx, Err: fmt.Errorf("index %d: SendDeposit 失败: %w", idx, err)}
	}

	return Result{
		Index:        idx,
		Hash:         txRes.TxHash,
		Err:          nil,
		Nonce:        txRes.Nonce,
		UsedGas:      txRes.UsedGas,
		EstimatedGas: txRes.EstimatedGas,
		BlockNumber:  txRes.BlockNumber,
		BlockHash:    txRes.BlockHash,
	}
}

// ---------------- 工具函数 ----------------

func readJson(path string) ([]JsonItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var arr []JsonItem
	dec := json.NewDecoder(f)
	if err := dec.Decode(&arr); err != nil {
		return nil, fmt.Errorf("解析 JSON 数组失败: %w", err)
	}
	if len(arr) == 0 {
		return nil, errors.New("JSON 数组为空")
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
	if limit >= 0 {
		if start+limit < end {
			end = start + limit
		}
	}
	return in[start:end]
}

func decideAmount(amountWeiStr string, amountETH float64) (*big.Int, error) {
	if strings.TrimSpace(amountWeiStr) != "" {
		z := new(big.Int)
		_, ok := z.SetString(strings.TrimSpace(amountWeiStr), 10)
		if !ok {
			return nil, fmt.Errorf("无法解析 --amount-wei=%s", amountWeiStr)
		}
		if z.Sign() <= 0 {
			return nil, fmt.Errorf("amount-wei 必须 > 0")
		}
		return z, nil
	}
	if amountETH <= 0 {
		return nil, fmt.Errorf("amount-eth 必须 > 0")
	}
	// ETH -> Wei：ETH * 1e18
	eth := big.NewFloat(amountETH)
	weiPerEth := new(big.Float).SetInt(big.NewInt(1_000_000_000_000_000_000)) // 1e18
	weiF := new(big.Float).Mul(eth, weiPerEth)
	z := new(big.Int)
	weiF.Int(z) // 向下取整即可
	if z.Sign() <= 0 {
		return nil, fmt.Errorf("换算后的 Wei 非法")
	}
	return z, nil
}

func gweiF(v float64) *big.Int {
	// Gwei -> Wei：1e9
	f := big.NewFloat(v)
	unit := new(big.Float).SetInt(big.NewInt(1_000_000_000)) // 1e9
	w := new(big.Float).Mul(f, unit)
	z := new(big.Int)
	w.Int(z)
	return z
}

func printResult(r Result) {
	prefix := fmt.Sprintf("[#%d]", r.Index)
	if r.Err != nil {
		log.Printf("%s ❌ 失败: %v", prefix, r.Err)
		return
	}
	log.Printf("%s ✅ 成功: tx=%s nonce=%d gasUsed=%d estGas=%d block=%d(%s)",
		prefix, r.Hash, r.Nonce, r.UsedGas, r.EstimatedGas, r.BlockNumber, r.BlockHash)
}
