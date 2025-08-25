package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"time"

	// 改成你的真实模块路径
	"n42-test/internal/deposit"
)

// ======= 固定配置（按你的本地链替换）=======
const (
	RPC      = "http://127.0.0.1:8545"
	CONTRACT = "0x5FbDB2315678afecb367f032d93F642f64180aa3" // 本地/测试链 Deposit 合约地址
)

// 0x01：ETH1 地址型提现凭证
// wc = 0x01 || 11*0x00 || address(20B)
func computeWithdrawalCredentialsFromEth1(executionAddressHex string) (string, error) {
	addrBytes, err := hex.DecodeString(strings.TrimPrefix(executionAddressHex, "0x"))
	if err != nil {
		return "", fmt.Errorf("decode address hex failed: %w", err)
	}
	if len(addrBytes) != 20 {
		return "", fmt.Errorf("execution address must be 20 bytes, got %d", len(addrBytes))
	}
	var wc [32]byte
	wc[0] = 0x01
	copy(wc[12:], addrBytes) // 直接粘贴 20 字节地址
	return "0x" + hex.EncodeToString(wc[:]), nil
}

// —— 输入辅助 —— //
var in = bufio.NewReader(os.Stdin)

func readLine(prompt string) string {
	fmt.Print(prompt)
	s, _ := in.ReadString('\n')
	return strings.TrimSpace(s)
}

func readHexWithLen(prompt string, wantBytes int) string {
	for {
		s := readLine(prompt)
		if s == "" {
			fmt.Println("⚠️ 不能为空")
			continue
		}
		if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
			s = "0x" + s
		}
		hexPart := s[2:]
		if len(hexPart)%2 != 0 {
			fmt.Println("⚠️ 十六进制长度必须为偶数")
			continue
		}
		b, err := hex.DecodeString(hexPart)
		if err != nil {
			fmt.Printf("⚠️ 非法十六进制：%v\n", err)
			continue
		}
		if len(b) != wantBytes {
			fmt.Printf("⚠️ 长度不对：期望 %d 字节，实际 %d 字节\n", wantBytes, len(b))
			continue
		}
		return s
	}
}

// 读取 ETH 金额（可小数），转换为 (gwei uint64, wei *big.Int)
func readAmountETH(prompt string, def string) (uint64, *big.Int) {
	for {
		s := readLine(prompt)
		if s == "" {
			s = def
		}
		// 解析小数 ETH
		f, ok := new(big.Float).SetString(s)
		if !ok {
			fmt.Println("⚠️ 请输入合法的数字（可小数）")
			continue
		}
		if f.Sign() <= 0 {
			fmt.Println("⚠️ 金额必须 > 0")
			continue
		}

		// ETH -> gwei（× 1e9），要求能表示为整数 gwei
		gweiF := new(big.Float).Mul(f, big.NewFloat(1e9))
		gweiInt := new(big.Int)
		_, _ = gweiF.Int(gweiInt) // 向下取整，返回值丢弃

		// 校验没有小数残留（保证 gwei 精度准确）
		diff := new(big.Float).Sub(gweiF, new(big.Float).SetInt(gweiInt))
		if diff.Cmp(big.NewFloat(0)) != 0 {
			fmt.Println("⚠️ 金额需精确到 1 gwei（小数位需满足 9 位以内且不产生残留），请重试")
			continue
		}
		// 检查范围
		if !gweiInt.IsUint64() {
			fmt.Println("⚠️ 金额过大（gwei 溢出 uint64）")
			continue
		}
		gwei := gweiInt.Uint64()

		// wei = gwei * 1e9
		wei := new(big.Int).Mul(gweiInt, big.NewInt(1_000_000_000))
		return gwei, wei
	}
}

func main() {
	fmt.Println("=== 交互式质押（Deposit）===")
	fmt.Printf("固定 RPC: %s\n固定合约: %s\n\n", RPC, CONTRACT)

	// 1) 输入参数
	senderSK := readHexWithLen("1) 发送账户私钥(EOA 32B 0x…): ", 32)
	blsSK := readHexWithLen("2) 验证者 BLS 私钥(32B 0x…): ", 32)
	pubkeyHex := readHexWithLen("3) 验证者 BLS 公钥(48B 0x…): ", 48)
	withdrawAddr := readHexWithLen("4) 提现地址(执行层地址 20B 0x…): ", 20)
	amtGwei, amtWei := readAmountETH("5) 质押金额(单位 ETH，可小数；默认 32): ", "32")

	// 2) 计算 withdrawal_credentials (0x01)
	wcHex, err := computeWithdrawalCredentialsFromEth1(withdrawAddr)
	if err != nil {
		log.Fatalf("计算提现凭证失败: %v", err)
	}

	// 3) 计算签名 & root（正确）
	correctSigHex, correctRootHex, err := deposit.ComputeDepositSignatureAndRoot(pubkeyHex, wcHex, amtGwei, blsSK)
	if err != nil {
		log.Fatalf("计算签名失败: %v", err)
	}
	fmt.Println("\n=== 计算完成 ===")
	fmt.Println("withdrawal_credentials:", wcHex)
	fmt.Println("signature:", correctSigHex)
	fmt.Println("root     :", correctRootHex)

	// 4) 组装交易参数（Nonce/Gas 自动）
	params := &deposit.DepositParams{
		Contract:             CONTRACT,
		PrivateKeyHex:        senderSK,
		RPC:                  RPC,
		PubkeyHex:            pubkeyHex,
		WCHex:                wcHex,
		SignatureHex:         correctSigHex,
		RootHex:              correctRootHex,
		AmountWei:            amtWei,
		Nonce:                -1,
		GasLimit:             0,
		MaxPriorityFeePerGas: nil,
		MaxFeePerGas:         nil,
	}

	// 5) 发送交易
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cli, err := deposit.NewClient(ctx, RPC, senderSK)
	if err != nil {
		log.Fatalf("NewClient 失败: %v", err)
	}
	defer cli.Close()

	cli.DebugPrintAccountState(ctx)

	txRes, err := cli.SendDeposit(ctx, params)
	if err != nil {
		log.Fatalf("发送失败: %v", err)
	}

	// 6) 输出结果
	fmt.Println("\n=== 交易结果 ===")
	fmt.Printf("TxHash=%s\nNonce=%d\nEstGas=%d\nUsedGas=%d\nBlockNumber=%d\nEth1BlockHash=%s\n",
		txRes.TxHash, txRes.Nonce, txRes.EstimatedGas, txRes.UsedGas, txRes.BlockNumber, txRes.BlockHash)

	fmt.Println("\n[说明] 正常质押路径：稍后在 BeaconState 中该 pubkey 会进入激活流程。")
}
