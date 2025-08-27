package validator

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"time"
)

// ValidateStreamFiltered 启动 ./mobile-sdk-test validate 并“实时”筛选关键输出
// rpcURL 为空则不设置环境变量；非空则注入 RPC_URL=...
func ValidateStreamFiltered(ctx context.Context, validatorPrivHex string, rpcURL string) error {
	bin := "./mobile-sdk-test"

	args := []string{"validate", "--validator-private-key", validatorPrivHex}

	cmd := exec.CommandContext(ctx, bin, args...)

	// 可选注入 RPC_URL（若你的二进制需要）
	if rpcURL != "" {
		cmd.Env = append(os.Environ(), "RPC_URL="+rpcURL)
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

	// 正则/关键字
	reConnected := regexp.MustCompile(`^Connected to .*`)
	reSubscribed := regexp.MustCompile(`Subscribed to 'subscribeToVerificationRequest'`)
	reVerify := regexp.MustCompile(`^verify,`)
	reSuccess := regexp.MustCompile(`^success,`)
	reSigResult := regexp.MustCompile(`sig verify result:\s*(\S+)`)
	reComputedStateRoot := regexp.MustCompile(`^Computed state_root from genesis alloc:`)
	reReceiptsRootLine := regexp.MustCompile(`^receipts_root:\s*(0x[0-9a-fA-F]{64})$`)
	reComputedHex := regexp.MustCompile(`^computed\s+(0x[0-9a-fA-F]{64})$`)
	reReceivedBlock := regexp.MustCompile(`^Received block:`)

	// 这些从“Received block: ... Header { ... }”长行里再抽取关键信息
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
		// 默认 buf 不够长，放大到 1MB，容纳超长 Header 行
		buf := make([]byte, 0, 1024)
		sc.Buffer(buf, 1024*1024)

		for sc.Scan() {
			line := sc.Text()

			switch {
			case reConnected.MatchString(line):
				printTS(line)
			case reSubscribed.MatchString(line):
				printTS(line)
			case reReceivedBlock.MatchString(line):
				// 从超长行中提取关键信息
				number := firstSub(reNum, line)
				parent := firstSub(reParent, line)
				state := firstSub(reState, line)
				rroot := firstSub(reReceipt, line)
				req := firstSub(reReq, line)

				printTS(fmt.Sprintf("Received block summary => number=%s parent_hash=%s", emptyDash(number), emptyDash(parent)))
				if state != "" {
					printTS("  state_root=" + state)
				}
				if rroot != "" {
					printTS("  receipts_root=" + rroot)
				}
				if req != "" {
					printTS("  requests_hash=" + req)
				}

			case reVerify.MatchString(line):
				printTS(line)
			case reSuccess.MatchString(line):
				printTS(line)
			case reSigResult.MatchString(line):
				printTS(line)
			case reComputedStateRoot.MatchString(line):
				printTS(line)
			case reComputedHex.MatchString(line):
				printTS(line)
			case reReceiptsRootLine.MatchString(line):
				printTS(line)
				// 其他行忽略
			}
		}
		if err := sc.Err(); err != nil {
			printTS(fmt.Sprintf("stdout scanner error: %v", err))
		}
	}()

	// 实时读取 stderr（只在非空时打印一行提示，避免刷屏）
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
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("validate exit: %w", err)
	}
	return nil
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
