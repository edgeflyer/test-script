package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/joho/godotenv"
)

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("env %s not set", k)
	}
	return v
}

func mustHexBig(s string) *big.Int {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if s == "" {
		return big.NewInt(0)
	}
	n := new(big.Int)
	_, ok := n.SetString(s, 16)
	if !ok {
		log.Fatalf("parse hex big.Int failed: %s", s)
	}
	return n
}

func main() {
	// === 相当于 ethers.getDefaultProvider(process.env.RPC_URL) ===
	_ = godotenv.Load()
	rpcURL := mustEnv("RPC_URL")
	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		log.Fatalf("dial rpc: %v", err)
	}
	defer client.Close()

	ctx := context.Background()

	// === 资金接收地址（exitDeploySenderAddress） ===
	exitDeploySenderAddress := common.HexToAddress("0x8646861A7cF453dDD086874d622b0696dE5b9674")

	// 查询接收地址余额（balance）
	bal, err := client.BalanceAt(ctx, exitDeploySenderAddress, nil)
	if err != nil {
		log.Fatalf("get balance: %v", err)
	}
	fmt.Println("balance (wei):", bal.String())

	// === 用 .env 里的 PRIVATE_KEY 账户转 1 ETH 给接收地址 ===
	privHex := mustEnv("PRIVATE_KEY")
	privHex = strings.TrimPrefix(strings.TrimPrefix(privHex, "0x"), "0X")
	privKey, err := crypto.HexToECDSA(privHex)
	if err != nil {
		log.Fatalf("parse PRIVATE_KEY: %v", err)
	}
	from := crypto.PubkeyToAddress(privKey.PublicKey)
	fmt.Println("Sender account:", from.Hex())

	// 查询发送方余额
	fromBal, err := client.BalanceAt(ctx, from, nil)
	if err != nil {
		log.Fatalf("get sender balance: %v", err)
	}
	fmt.Printf("Sender balance before: %s ETH\n", weiToEth(fromBal))

	// 组装一笔 native ETH 转账（1 ETH）
	amountToSendWei := new(big.Int).Mul(big.NewInt(1), big.NewInt(1e18))
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		log.Fatalf("get nonce: %v", err)
	}
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		log.Fatalf("suggest gas price: %v", err)
	}
	// 简单估个 gasLimit
	msg := ethereum.CallMsg{From: from, To: &exitDeploySenderAddress, Value: amountToSendWei}
	gasLimit, err := client.EstimateGas(ctx, msg)
	if err != nil {
		// 本地链给个兜底
		gasLimit = 21000
	}

	tx := types.NewTransaction(nonce, exitDeploySenderAddress, amountToSendWei, gasLimit, gasPrice, nil)

	chainID, err := client.NetworkID(ctx)
	if err != nil {
		log.Fatalf("network id: %v", err)
	}
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), privKey)
	if err != nil {
		log.Fatalf("sign tx: %v", err)
	}
	fmt.Println("Sending transaction...")
	if err := client.SendTransaction(ctx, signedTx); err != nil {
		log.Fatalf("send tx: %v", err)
	}
	fmt.Println("Transaction hash:", signedTx.Hash().Hex())

	// 等待出块
	receipt, err := waitMined(ctx, client, signedTx.Hash())
	if err != nil {
		log.Fatalf("wait mined: %v", err)
	}
	if receipt.Status == types.ReceiptStatusSuccessful {
		fmt.Printf("Transaction confirmed in block: %d\n", receipt.BlockNumber.Uint64())
		fmt.Printf("Gas used: %s\n", receipt.GasUsed)
	} else {
		log.Fatalf("tx reverted, status=%d", receipt.Status)
	}

	// === 查询接收地址当前 nonce ===
	currentNonce, err := client.NonceAt(ctx, exitDeploySenderAddress, nil)
	if err != nil {
		log.Fatalf("get receiver nonce: %v", err)
	}
	fmt.Printf("exitDeploySenderAddress %s current nonce: %d\n", exitDeploySenderAddress.Hex(), currentNonce)

	// === 下面是按你给的 transactionData 构造并广播预签名 raw legacy tx（部署合约） ===
	// 你的原数据（保持一致）：
	transactionDataNonceHex := "0x0"
	transactionDataGasPriceHex := "0xe8d4a51000"
	transactionDataGasLimitHex := "0x3d090"
	transactionDataDataHex := "0x7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff5f556101f880602d5f395ff33373fffffffffffffffffffffffffffffffffffffffe1460cb5760115f54807fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff146101f457600182026001905f5b5f82111560685781019083028483029004916001019190604d565b909390049250505036603814608857366101f457346101f4575f5260205ff35b34106101f457600154600101600155600354806003026004013381556001015f35815560010160203590553360601b5f5260385f601437604c5fa0600101600355005b6003546002548082038060101160df575060105b5f5b8181146101835782810160030260040181604c02815460601b8152601401816001015481526020019060020154807fffffffffffffffffffffffffffffffff00000000000000000000000000000000168252906010019060401c908160381c81600701538160301c81600601538160281c81600501538160201c81600401538160181c81600301538160101c81600201538160081c81600101535360010160e1565b910180921461019557906002556101a0565b90505f6002555f6003555b5f54807fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff14156101cd57505f5b6001546002828201116101e25750505f6101e8565b01600290035b5f555f600155604c025ff35b5f5ffd"
	transactionDataVHex := "0x1b"
	transactionDataRHex := "0x539"
	transactionDataSHex := "0x5feeb084551e4e03a3581e269bc2ea2f8d0008"

	// 解析 hex
	txNonce := mustHexBig(transactionDataNonceHex).Uint64()
	txGasPrice := mustHexBig(transactionDataGasPriceHex)
	txGasLimit := mustHexBig(transactionDataGasLimitHex).Uint64()
	txDataBytes, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(transactionDataDataHex, "0x"), "0X"))
	if err != nil {
		log.Fatalf("decode data: %v", err)
	}
	v := mustHexBig(transactionDataVHex)
	r := mustHexBig(transactionDataRHex)
	s := mustHexBig(transactionDataSHex)

	// 提示：如果 raw tx 的 nonce 与链上当前 nonce 不一致，提示风险
	if txNonce != currentNonce {
		fmt.Printf("⚠️ Warning: Provided transaction nonce (%d) does not match receiver's current nonce (%d).\n", txNonce, currentNonce)
	}

	// 组装“预签名”LegacyTx（to=nil 表示部署合约）
	signedLegacy := types.NewTx(&types.LegacyTx{
		Nonce:    txNonce,
		GasPrice: txGasPrice,
		Gas:      txGasLimit,
		To:       nil,
		Value:    big.NewInt(0),
		Data:     txDataBytes,
		V:        v,
		R:        r,
		S:        s,
	})

	// 广播 raw tx（无需再次签名）
	if err := client.SendTransaction(ctx, signedLegacy); err != nil {
		log.Fatalf("send raw legacy tx: %v", err)
	}
	fmt.Println("Raw legacy tx sent:", signedLegacy.Hash().Hex())

	rawRcpt, err := waitMined(ctx, client, signedLegacy.Hash())
	if err != nil {
		log.Fatalf("wait raw tx: %v", err)
	}
	if rawRcpt.Status == types.ReceiptStatusSuccessful {
		fmt.Printf("🎉 Raw tx confirmed in block %d, gasUsed %s\n", rawRcpt.BlockNumber.Uint64(), rawRcpt.GasUsed)
	} else {
		log.Fatalf("raw tx reverted, status=%d", rawRcpt.Status)
	}
}

// 简单等确认
func waitMined(ctx context.Context, c *ethclient.Client, hash common.Hash) (*types.Receipt, error) {
	for {
		rcpt, err := c.TransactionReceipt(ctx, hash)
		if err == nil && rcpt != nil {
			return rcpt, nil
		}
		time.Sleep(800 * time.Millisecond)
	}
}

// 小工具：wei → ETH 字符串
func weiToEth(wei *big.Int) string {
	if wei == nil {
		return "0"
	}
	// 仅作为人类可读输出
	f := new(big.Float).Quo(new(big.Float).SetInt(wei), big.NewFloat(1e18))
	return f.Text('f', 6)
}
