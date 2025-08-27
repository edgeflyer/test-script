package validator

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	// 修改为你项目里 beaconext 的实际导入路径
	"n42-test/internal/beaconext"
)

// ValidateStreamFiltered 启动 ./mobile-sdk-test validate 并实时筛选关键输出；
// 收到块后，通过 HTTP RPC (eth_getBlockByNumber) 查询该高度的 eth1 区块哈希。
// wsURL:  验证者订阅用 WS 端点（如 ws://127.0.0.1:8546），仅注入给二进制。
// httpURL: 执行层 HTTP RPC 端点（如 http://127.0.0.1:8545），用于区块查询。
func ValidateStreamFiltered(ctx context.Context, validatorPrivHex string, wsURL string, httpURL string) error {
	bin := "./mobile-sdk-test"
	args := []string{"validate", "--validator-private-key", validatorPrivHex}

	cmd := exec.CommandContext(ctx, bin, args...)

	// 注入 WS 地址给二进制（用于订阅）
	if wsURL != "" {
		cmd.Env = append(os.Environ(), "RPC_URL="+wsURL)
	} else {
		cmd.Env = os.Environ()
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start validate: %w", err)
	}

	// ===== HTTP RPC 客户端（查询区块哈希）=====
	var ethCli *beaconext.Client
	if httpURL != "" {
		ethCli = beaconext.NewClient(httpURL)
	}

	// 关键行匹配
	reConnected := regexp.MustCompile(`^Connected to (.+)$`)
	reSubscribed := regexp.MustCompile(`Subscribed to 'subscribeToVerificationRequest'`)
	reSuccess := regexp.MustCompile(`^success,`)
	reSigResult := regexp.MustCompile(`sig verify result:\s*(\S+)`)
	reComputedStateRoot := regexp.MustCompile(`^Computed state_root from genesis alloc:`)
	reReceiptsRootLine := regexp.MustCompile(`^receipts_root:\s*(0x[0-9a-fA-F]{64})$`)
	reComputedHex := regexp.MustCompile(`^computed\s+(0x[0-9a-fA-F]{64})$`)
	reReceivedBlock := regexp.MustCompile(`^Received block:`)
	// 注意：不打印超长的 verify, ...

	// 从“Received block”长行中抽取字段
	reNum := regexp.MustCompile(`\bnumber:\s*(\d+)`)
	reParent := regexp.MustCompile(`\bparent_hash:\s*(0x[0-9a-fA-F]{64})`)
	reState := regexp.MustCompile(`\bstate_root:\s*(0x[0-9a-fA-F]{64})`)
	reReceipt := regexp.MustCompile(`\breceipts_root:\s*(0x[0-9a-fA-F]{64})`)
	reReq := regexp.MustCompile(`\brequests_hash:\s*Some\((0x[0-9a-fA-F]{64})\)`)

	printTS := func(s string) {
		fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), s)
	}

	// 实时读取 stdout
	go func() {
		sc := bufio.NewScanner(stdout)
		// 扩大 buffer 以容纳超长 Header 行
		buf := make([]byte, 0, 1024)
		sc.Buffer(buf, 1024*1024)

		for sc.Scan() {
			line := sc.Text()

			switch {
			case reConnected.MatchString(line):
				// 连接到执行层 WS
				m := reConnected.FindStringSubmatch(line)
				if len(m) >= 2 {
					printTS(fmt.Sprintf("Connected to execution node %s", strings.TrimSpace(m[1])))
				} else {
					printTS(line)
				}

			case reSubscribed.MatchString(line):
				// 订阅验证请求流成功
				printTS("Subscribed to verification request stream")

			case reReceivedBlock.MatchString(line):
				// 收到待验证区块，抽取关键信息
				number := firstSub(reNum, line)
				parent := firstSub(reParent, line)
				state := firstSub(reState, line)
				rroot := firstSub(reReceipt, line)
				req := firstSub(reReq, line)

				// 单独打印块号
				printTS(fmt.Sprintf("Block #%s", emptyDash(number)))

				// 打印头部摘要
				printTS(fmt.Sprintf("  parent_hash = %s", emptyDash(parent)))
				if state != "" {
					printTS("  state_root = " + state)
				}
				if rroot != "" {
					printTS("  receipts_root = " + rroot)
				}
				if req != "" {
					printTS("  requests_hash = " + req)
				}

				// 通过 HTTP RPC 查询 eth1 的区块哈希（等待 HTTP 节点追上 & 重试）
				if ethCli != nil && number != "" {
					if h, err := queryEth1HashByNumberWait(ctx, ethCli, number, httpURL); err == nil && h != "" {
						printTS(fmt.Sprintf("Eth1 block hash (via RPC@%s) = %s", httpURL, h))
					} else if err != nil {
						printTS(fmt.Sprintf("Eth1 block hash query failed: %v", err))
					}
				}

			case reSuccess.MatchString(line):
				// 执行成功（压缩显示详细内容）
				printTS("Block execution success (details: " + trimAfter(line, "success,") + ")")

			case reSigResult.MatchString(line):
				// BLS 签名验证结果
				printTS(line)
				fmt.Println("------------------------------------------")

			case reComputedStateRoot.MatchString(line):
				// 基于创世分配计算出的 state_root（用于比对）
				printTS(line)

			case reComputedHex.MatchString(line):
				// 通常是本地重算的 receipts_root
				printTS(line)

			case reReceiptsRootLine.MatchString(line):
				// 区块头里的 receipts_root
				printTS(line)

				// 其余行忽略（尤其是不打印超长的 verify, ...）
			}
		}
		if err := sc.Err(); err != nil {
			printTS(fmt.Sprintf("stdout scanner error: %v", err))
		}
	}()

	// 实时读取 stderr（有内容就打印）
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if len(line) > 0 {
				printTS("[stderr] " + line)
			}
		}
	}()

	// 等待进程退出
	waitErr := cmd.Wait()
	// 结束时加一条分割线，便于阅读
	fmt.Println("-------------------------------------------------------------")
	if waitErr != nil {
		return fmt.Errorf("validate exit: %w", waitErr)
	}
	return nil
}

// 等待 HTTP 节点追上目标高度后，再查询该高度的区块哈希。
// - 先轮询 latest（通过 tag="latest"），若 latest < 目标块高，则等待；
// - 当 latest >= 目标块高时，再对该高度做多次重试查询；
// - 都失败则返回最后一次错误。
func queryEth1HashByNumberWait(ctx context.Context, cli *beaconext.Client, numberDec string, httpURL string) (string, error) {
	target, err := strconv.ParseUint(numberDec, 10, 64)
	if err != nil {
		return "", fmt.Errorf("parse block number '%s': %w", numberDec, err)
	}

	// 1) 等待 latest >= target
	const (
		latestPollInterval = 200 * time.Millisecond
		latestMaxWait      = 60 * time.Second
	)
	deadlineLatest := time.Now().Add(latestMaxWait)

	for {
		latest, err := getLatestNumber(ctx, cli)
		if err == nil {
			if latest >= target {
				break
			}
			// 提示 HTTP 节点还没追上
			//（只在首次或每秒打印一次以免刷屏）
			// printEverySec(fmt.Sprintf("HTTP node @%s latest=%d, waiting to reach target=%d ...", httpURL, latest, target))
		} else {
			// latest 获取失败也继续短暂等待后重试
			printEverySec(fmt.Sprintf("HTTP node @%s latest query error: %v (will retry)", httpURL, err))
		}

		if time.Now().After(deadlineLatest) {
			return "", fmt.Errorf("http node did not catch up to %d within %s", target, latestMaxWait)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(latestPollInterval):
		}
	}

	// 2) 查询目标高度的 hash（多次重试）
	const (
		attempts = 20
		backoff  = 250 * time.Millisecond
	)
	tag := "0x" + strconv.FormatUint(target, 16)

	var lastErr error
	for i := 0; i < attempts; i++ {
		blk, err := cli.EthGetBlockByNumber(ctx, tag, false)
		if err == nil && blk != nil && blk.Hash != "" && blk.Hash != "0x" {
			return blk.Hash, nil
		}
		if err == nil {
			lastErr = fmt.Errorf("empty result")
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
	}
	return "", lastErr
}

// 查询 latest 的区块号（十六进制转为十进制）
func getLatestNumber(ctx context.Context, cli *beaconext.Client) (uint64, error) {
	blk, err := cli.EthGetBlockByNumber(ctx, "latest", false)
	if err != nil {
		return 0, err
	}
	if blk == nil || blk.Number == "" || blk.Number == "0x" {
		return 0, fmt.Errorf("empty latest block")
	}
	// blk.Number 是十六进制数量（0x...）
	trim := strings.TrimPrefix(blk.Number, "0x")
	if trim == "" {
		return 0, fmt.Errorf("bad latest number: %s", blk.Number)
	}
	u, err := strconv.ParseUint(trim, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parse latest '%s': %w", blk.Number, err)
	}
	return u, nil
}

var lastPrintSecond int64

// 每秒最多打印一次提示，避免刷屏
func printEverySec(s string) {
	now := time.Now().Unix()
	if now != lastPrintSecond {
		lastPrintSecond = now
		fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), s)
	}
}

func firstSub(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// 把某个前缀之后的内容取出来（用于压缩 success, 后面的冗长信息）
func trimAfter(s, prefix string) string {
	i := strings.Index(s, prefix)
	if i < 0 {
		return s
	}
	return strings.TrimSpace(s[i+len(prefix):])
}
