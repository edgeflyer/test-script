package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"n42-test/internal/exit" // 你自己的工具包
)

// 把私钥 hex 字符串转成 *ecdsa.PrivateKey
func mustPriv(hexkey string) *ecdsa.PrivateKey {
	k := hexkey
	if len(k) >= 2 && k[:2] == "0x" {
		k = k[2:]
	}
	priv, err := crypto.HexToECDSA(k)
	if err != nil {
		log.Fatalf("bad privkey: %v", err)
	}
	return priv
}

func main() {
	// RPC 节点
	rpc := "http://127.0.0.1:8545"
	cli, err := ethclient.Dial(rpc)
	if err != nil {
		log.Fatal(err)
	}
	defer cli.Close()

	// 合约地址（你给的）
	contract := common.HexToAddress("0x00000961Ef480Eb55e80D19ad83579A64c007002")

	// 这里手动输入EOA私钥（示例），不要用环境变量
	privHex := "0x798bb244fd5d8f255b080692d769bebb0f25f5828b6ade1889502f5616e5dbd7"
	priv := mustPriv(privHex)
	from := crypto.PubkeyToAddress(priv.PublicKey)
	fmt.Println("Using sender:", from.Hex())

	// 准备 pubkey (48字节 BLS 公钥)
	pubkeyHex := "84cb0739e67c7fefd6ad94a06d2fe76bfe9e5ac7db0f1b0992e97ef74fd5a77ff30b666d516343b474f1ca9a2a7fc084"
	pubkey, _ := hex.DecodeString(pubkeyHex)
	if len(pubkey) != 48 {
		log.Fatalf("pubkey must be 48 bytes, got %d", len(pubkey))
	}

	// 退出请求里的 amount 字段（8 字节大端），这里例子写 1 ETH
	amountWei := big.NewInt(0).Mul(big.NewInt(0), big.NewInt(1e18))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 发送退出请求
	tx, rcpt, err := exit.SendExitRequest(ctx, cli, priv, contract, pubkey, amountWei, true)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Tx hash:", tx.Hash().Hex())
	if rcpt != nil {
		fmt.Println("Status:", rcpt.Status, "Block:", rcpt.BlockNumber)
	}
}
