package exit

import (
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// PackExitCalldata 将 48 字节的 BLS 公钥 与 8 字节 amount(wei, 大端) 打包成 calldata:
// [pubkey(48) | amount(8)]
func PackExitCalldata(pubkey48 []byte, amountWei *big.Int) ([]byte, error) {
	if len(pubkey48) != 48 {
		return nil, fmt.Errorf("pubkey length must be 48, got %d", len(pubkey48))
	}
	if amountWei == nil || amountWei.Sign() < 0 {
		return nil, errors.New("amount must be non-negative")
	}

	// amount 需要 8 字节无符号整数（大端）。若超出 2^64-1，报错。
	if amountWei.BitLen() > 64 {
		return nil, fmt.Errorf("amount too large for 8-byte field (bitlen=%d)", amountWei.BitLen())
	}
	amountU64 := amountWei.Uint64()

	data := make([]byte, 0, 56)
	data = append(data, pubkey48...)
	amt := make([]byte, 8)
	binary.BigEndian.PutUint64(amt, amountU64)
	data = append(data, amt...)
	return data, nil
}

// GetExitFee 读取当前区块的退出请求费用（wei）。
// 调用规则：对合约做一次无 calldata 的 eth_call，返回 32 字节整数。
func GetExitFee(ctx context.Context, cli *ethclient.Client, contract common.Address) (*big.Int, error) {
	// 空 data，value=0
	call := ethereum.CallMsg{
		From:  common.Address{}, // 任意
		To:    &contract,
		Value: big.NewInt(0),
		Data:  nil,
	}
	out, err := cli.CallContract(ctx, call, nil)
	if err != nil {
		return nil, fmt.Errorf("eth_call get fee: %w", err)
	}
	if len(out) == 0 {
		// 有些实现可能返回空，按 0 处理（但正常应返回 32 字节）
		return big.NewInt(0), nil
	}
	fee := new(big.Int).SetBytes(out) // 32 字节大数
	return fee, nil
}

// SendExitRequest 发送退出请求交易：
// 1) 读取当前费用；2) 估算 gas；3) 组装 EIP-1559 或回退 legacy；4) 签名发送；5) 可选等待上链。
// —— 修复点：使用 crypto.PubkeyToAddress 获取正确 from；若 "nonce too low" 则刷新 nonce 重试一次。
func SendExitRequest(
	ctx context.Context,
	cli *ethclient.Client,
	priv *ecdsa.PrivateKey,
	contract common.Address,
	pubkey48 []byte,
	amountWei *big.Int,
	wait bool,
) (*types.Transaction, *types.Receipt, error) {

	// 修复：正确获取 from 地址
	from := crypto.PubkeyToAddress(priv.PublicKey)

	// 1) 读取费用
	fee, err := GetExitFee(ctx, cli, contract)
	if err != nil {
		return nil, nil, err
	}
	if fee.Sign() <= 0 {
		return nil, nil, fmt.Errorf("exit fee invalid: %s", fee.String())
	}

	// 2) 打包 calldata
	calldata, err := PackExitCalldata(pubkey48, amountWei)
	if err != nil {
		return nil, nil, err
	}

	// 3) 估算 gas（写路径要带 value）
	estGas, err := cli.EstimateGas(ctx, ethereum.CallMsg{
		From:  from,
		To:    &contract,
		Value: fee,
		Data:  calldata,
	})
	if err != nil {
		estGas = 150_000
	}

	// 公共参数
	chainID, err := cli.NetworkID(ctx)
	if err != nil {
		return nil, nil, err
	}
	makeLegacy := func(nonce uint64) (*types.Transaction, error) {
		gp, gperr := cli.SuggestGasPrice(ctx)
		if gperr != nil {
			return nil, fmt.Errorf("suggest gas: %v", gperr)
		}
		return types.NewTx(&types.LegacyTx{
			Nonce:    nonce,
			To:       &contract,
			Value:    fee,
			Gas:      estGas,
			GasPrice: gp,
			Data:     calldata,
		}), nil
	}
	make1559 := func(nonce uint64) (*types.Transaction, error) {
		tipCap, tipErr := cli.SuggestGasTipCap(ctx)
		if tipErr != nil {
			tipCap = big.NewInt(1_000_000_000) // 1 gwei 兜底
		}
		h, herr := cli.HeaderByNumber(ctx, nil)
		if herr != nil || h.BaseFee == nil {
			return makeLegacy(nonce) // 回退 legacy
		}
		feeCap := new(big.Int).Mul(h.BaseFee, big.NewInt(2))
		feeCap.Add(feeCap, tipCap)
		return types.NewTx(&types.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     nonce,
			To:        &contract,
			Value:     fee,
			Gas:       estGas,
			GasTipCap: tipCap,
			GasFeeCap: feeCap,
			Data:      calldata,
		}), nil
	}

	// 封装一次发送逻辑（带签名）
	sendOnce := func(nonce uint64) (*types.Transaction, error) {
		tx, mkErr := make1559(nonce)
		if mkErr != nil {
			return nil, mkErr
		}
		// Cancun 签名（若你的链更老，可改为 London/EIP155）
		signed, sErr := types.SignTx(tx, types.NewCancunSigner(chainID), priv)
		if sErr != nil {
			return nil, sErr
		}
		if err := cli.SendTransaction(ctx, signed); err != nil {
			return nil, err
		}
		return signed, nil
	}

	// 第一次用 pending nonce 发送
	nonce, err := cli.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, nil, err
	}
	signed, sendErr := sendOnce(nonce)
	if sendErr != nil && isNonceTooLow(sendErr) {
		// 刷新一次 nonce 再试（防止同时有别处发了同账户的交易）
		nonce2, nErr := cli.PendingNonceAt(ctx, from)
		if nErr != nil {
			return nil, nil, nErr
		}
		if nonce2 <= nonce {
			nonce2 = nonce + 1
		}
		signed, sendErr = sendOnce(nonce2)
	}
	if sendErr != nil {
		return nil, nil, sendErr
	}

	if !wait {
		return signed, nil, nil
	}
	rcpt, err := WaitMined(ctx, cli, signed.Hash())
	return signed, rcpt, err
}

func isNonceTooLow(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "nonce too low") ||
		strings.Contains(msg, "replacement transaction underpriced") ||
		strings.Contains(msg, "already known")
}

// WaitMined 轮询直到交易有回执（简单实现）。
func WaitMined(ctx context.Context, cli *ethclient.Client, txHash common.Hash) (*types.Receipt, error) {
	t := time.NewTicker(800 * time.Millisecond)
	defer t.Stop()

	for {
		rcpt, err := cli.TransactionReceipt(ctx, txHash)
		if err == nil && rcpt != nil {
			return rcpt, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func deriveAddress(priv *ecdsa.PrivateKey) common.Address {
	pub := priv.Public()
	pubKey, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return common.Address{}
	}
	return common.BigToAddress(pubKey.X) // 仅用于标识 from 时需避免；更稳妥是用 crypto.PubkeyToAddress
}
