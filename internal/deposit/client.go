// 可复用库：创建客户端、构造并发送交易
package deposit

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// deposit 函数 ABI（与以太坊存款合约一致）
const depositFuncABI = `
[{"inputs":[
	{"internalType":"bytes","name":"pubkey","type":"bytes"},
	{"internalType":"bytes","name":"withdrawal_credentials","type":"bytes"},
	{"internalType":"bytes","name":"signature","type":"bytes"},
	{"internalType":"bytes32","name":"deposit_data_root","type":"bytes32"}
],"name":"deposit","outputs":[],"stateMutability":"payable","type":"function"}]
`

type Client struct {
	cli        *ethclient.Client //客户端，负责RPC通道
	chainID    *big.Int
	fromAddr   common.Address
	privKey    *ecdsa.PrivateKey
	depositABI abi.ABI
}

// 新建客户端，用来连接RPC，解析私钥，获取链ID
func NewClient(ctx context.Context, rpcURL, privateKeyHex string) (*Client, error) {
	privateKeyHex = strings.TrimPrefix(privateKeyHex, "0x")
	// 转换成标准的*ecdsa.PrivateKey对象
	priv, err := crypto.HexToECDSA(privateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("parse private key failed: %w", err)
	}
	from := crypto.PubkeyToAddress(priv.PublicKey)

	cli, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("dial rpc failed: %w", err)
	}
	chainID, err := cli.NetworkID(ctx)
	if err != nil {
		return nil, fmt.Errorf("get network id failed: %w", err)
	}

	ab, err := abi.JSON(strings.NewReader(depositFuncABI))
	if err != nil {
		return nil, fmt.Errorf("parse deposit abi failed: %w", err)
	}

	return &Client{
		cli:        cli,
		chainID:    chainID,
		fromAddr:   from,
		privKey:    priv,
		depositABI: ab,
	}, nil
}

func (c *Client) Close() { c.cli.Close() }

// 工具：解 hex -> []byte（校验长度）
func mustDecodeHex(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return nil, fmt.Errorf("empty hex")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode hex failed: %w", err)
	}
	return b, nil
}

// 不做严格长度校验，错误数据也能上链
func buildDepositArgs(p *DepositParams) (pubkey, wc, sig []byte, root [32]byte, err error) {
	if p == nil {
		err = fmt.Errorf("nil params")
		return
	}
	pubkey, err = mustDecodeHex(p.PubkeyHex)
	if err != nil {
		return
	}
	// 常见为 48 字节；也有 96（压缩/非压缩差异）。这里放宽只检查 >=48
	if l := len(pubkey); l != 48 && l != 96 {
		err = ErrInvalidPubkeyLen
		return
	}

	wc, err = mustDecodeHex(p.WCHex)
	if err != nil {
		return
	}

	sig, err = mustDecodeHex(p.SignatureHex)
	if err != nil {
		return
	}

	rootBytes, err := mustDecodeHex(p.RootHex)
	if err != nil {
		return
	}
	// root 固定转成 [32]byte，超过 32 截断，不足 32 零填充
	copy(root[:], rootBytes)
	return
}

// SendDeposit 组装并发送 deposit 交易
func (c *Client) SendDeposit(ctx context.Context, p *DepositParams) (*TxResult, error) {
	if p.AmountWei == nil || p.AmountWei.Sign() <= 0 {
		return nil, fmt.Errorf("amount must be > 0 wei")
	}
	contract := common.HexToAddress(p.Contract)

	pubkey, wc, sig, root, err := buildDepositArgs(p)
	if err != nil {
		return nil, err
	}

	// ABI pack
	data, err := c.depositABI.Pack("deposit", pubkey, wc, sig, root)
	if err != nil {
		return nil, fmt.Errorf("abi pack failed: %w", err)
	}

	// nonce
	var nonce uint64
	if p.Nonce >= 0 {
		nonce = uint64(p.Nonce)
	} else {
		nonce, err = c.cli.PendingNonceAt(ctx, c.fromAddr)
		if err != nil {
			return nil, fmt.Errorf("get nonce failed: %w", err)
		}
	}

	// EIP-1559 fee
	var maxPriority, maxFee *big.Int
	if p.MaxPriorityFeePerGas != nil && p.MaxFeePerGas != nil {
		maxPriority = new(big.Int).Set(p.MaxPriorityFeePerGas)
		maxFee = new(big.Int).Set(p.MaxFeePerGas)
	} else {
		// 自动建议
		maxPriority, err = c.cli.SuggestGasTipCap(ctx)
		if err != nil {
			// 回退到旧接口
			gp, e2 := c.cli.SuggestGasPrice(ctx)
			if e2 != nil {
				return nil, fmt.Errorf("fee suggest failed: %v / %v", err, e2)
			}
			maxPriority = gp
			maxFee = new(big.Int).Mul(gp, big.NewInt(2))
		} else {
			// maxFee = baseFee + tip * 2，简化做法：用 tip 的若干倍兜底
			maxFee = new(big.Int).Mul(maxPriority, big.NewInt(20))
		}
	}

	// gas 估算
	gasLimit := p.GasLimit
	if gasLimit == 0 {
		call := ethereum.CallMsg{
			From:      c.fromAddr,
			To:        &contract,
			Gas:       0,
			GasPrice:  nil,
			GasFeeCap: maxFee,
			GasTipCap: maxPriority,
			Value:     p.AmountWei,
			Data:      data,
		}
		est, e := c.cli.EstimateGas(ctx, call)
		if e != nil {
			return nil, fmt.Errorf("estimate gas failed: %w", e)
		}
		// 稍加 buffer
		gasLimit = uint64(float64(est)*1.15) + 300000
	}

	// 构造 EIP-1559 动态费用交易
	txData := &gethtypes.DynamicFeeTx{
		ChainID:   c.chainID,
		Nonce:     nonce,
		To:        &contract,
		Value:     p.AmountWei,
		Data:      data,
		Gas:       gasLimit,
		GasTipCap: maxPriority,
		GasFeeCap: maxFee,
	}

	tx := gethtypes.NewTx(txData)

	// 签名并发送
	signer := gethtypes.LatestSignerForChainID(c.chainID)
	signedTx, err := gethtypes.SignTx(tx, signer, c.privKey)
	if err != nil {
		return nil, fmt.Errorf("sign tx failed: %w", err)
	}

	if err := c.cli.SendTransaction(ctx, signedTx); err != nil {
		return nil, fmt.Errorf("send tx failed: %w", err)
	}

	// 可选：等待上链（简单轮询）
	receipt, err := waitMined(ctx, c.cli, signedTx.Hash())
	if err != nil {
		return &TxResult{TxHash: signedTx.Hash().Hex(), EstimatedGas: gasLimit, Nonce: nonce}, fmt.Errorf("tx sent but waitMined failed: %w", err)
	}

	// 打印区块信息
	fmt.Printf("质押交易已上链!\n区块号: %s\n区块哈希: %s\n",
		receipt.BlockNumber.String(),
		receipt.BlockHash.Hex(),
	)

	return &TxResult{
		TxHash:       signedTx.Hash().Hex(),
		UsedGas:      receipt.GasUsed,
		Nonce:        nonce,
		EstimatedGas: gasLimit,
		BlockNumber:  receipt.BlockNumber.Uint64(),
		BlockHash:    receipt.BlockHash.Hex(),
	}, nil
}

func waitMined(ctx context.Context, cli *ethclient.Client, txHash common.Hash) (*gethtypes.Receipt, error) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	timeout := time.After(120 * time.Second) // 2 分钟兜底

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, fmt.Errorf("timeout waiting for receipt: %s", txHash.Hex())
		case <-t.C:
			rcpt, err := cli.TransactionReceipt(ctx, txHash)
			if err == nil && rcpt != nil {
				return rcpt, nil
			}
		}
	}
}

// 可并发批量发送（worker pool），后续你可从文件读入 items 调用此函数
type DepositItem struct {
	Params DepositParams
}

type DepositResult struct {
	Item   DepositItem
	Result *TxResult
	Err    error
}

func (c *Client) SendDepositsConcurrently(ctx context.Context, items []DepositItem, workers int) <-chan DepositResult {
	if workers <= 0 {
		workers = 4
	}
	in := make(chan DepositItem)
	out := make(chan DepositResult)

	// workers
	for w := 0; w < workers; w++ {
		go func() {
			for it := range in {
				res, err := c.SendDeposit(ctx, &it.Params)
				out <- DepositResult{Item: it, Result: res, Err: err}
			}
		}()
	}

	go func() {
		defer close(out)
		for _, it := range items {
			in <- it
		}
		close(in)
	}()

	return out
}

// Debug 辅助：打印当前账户余额/nonce
func (c *Client) DebugPrintAccountState(ctx context.Context) {
	nonce, _ := c.cli.PendingNonceAt(ctx, c.fromAddr)
	bal, _ := c.cli.BalanceAt(ctx, c.fromAddr, nil)
	log.Printf("From: %s Nonce: %d Balance(wei): %s", c.fromAddr.Hex(), nonce, bal.String())
}

// SendDepositNoWait 组装并发送 deposit 交易（不等待回执）
func (c *Client) SendDepositNoWait(ctx context.Context, p *DepositParams) (*TxResult, error) {
	if p.AmountWei == nil || p.AmountWei.Sign() <= 0 {
		return nil, fmt.Errorf("amount must be > 0 wei")
	}
	contract := common.HexToAddress(p.Contract)

	pubkey, wc, sig, root, err := buildDepositArgs(p)
	if err != nil {
		return nil, err
	}

	// ABI pack
	data, err := c.depositABI.Pack("deposit", pubkey, wc, sig, root)
	if err != nil {
		return nil, fmt.Errorf("abi pack failed: %w", err)
	}

	// nonce
	var nonce uint64
	if p.Nonce >= 0 {
		nonce = uint64(p.Nonce)
	} else {
		nonce, err = c.cli.PendingNonceAt(ctx, c.fromAddr)
		if err != nil {
			return nil, fmt.Errorf("get nonce failed: %w", err)
		}
	}

	// EIP-1559 fee（与原函数保持一致）
	var maxPriority, maxFee *big.Int
	if p.MaxPriorityFeePerGas != nil && p.MaxFeePerGas != nil {
		maxPriority = new(big.Int).Set(p.MaxPriorityFeePerGas)
		maxFee = new(big.Int).Set(p.MaxFeePerGas)
	} else {
		maxPriority, err = c.cli.SuggestGasTipCap(ctx)
		if err != nil {
			gp, e2 := c.cli.SuggestGasPrice(ctx)
			if e2 != nil {
				return nil, fmt.Errorf("fee suggest failed: %v / %v", err, e2)
			}
			maxPriority = gp
			maxFee = new(big.Int).Mul(gp, big.NewInt(2))
		} else {
			maxFee = new(big.Int).Mul(maxPriority, big.NewInt(20))
		}
	}

	// gas 估算
	gasLimit := p.GasLimit
	if gasLimit == 0 {
		call := ethereum.CallMsg{
			From:      c.fromAddr,
			To:        &contract,
			GasFeeCap: maxFee,
			GasTipCap: maxPriority,
			Value:     p.AmountWei,
			Data:      data,
		}
		est, e := c.cli.EstimateGas(ctx, call)
		if e != nil {
			return nil, fmt.Errorf("estimate gas failed: %w", e)
		}
		gasLimit = uint64(float64(est)*1.15) + 300000
	}

	// 构造并签名
	tx := gethtypes.NewTx(&gethtypes.DynamicFeeTx{
		ChainID:   c.chainID,
		Nonce:     nonce,
		To:        &contract,
		Value:     p.AmountWei,
		Data:      data,
		Gas:       gasLimit,
		GasTipCap: maxPriority,
		GasFeeCap: maxFee,
	})
	signer := gethtypes.LatestSignerForChainID(c.chainID)
	signedTx, err := gethtypes.SignTx(tx, signer, c.privKey)
	if err != nil {
		return nil, fmt.Errorf("sign tx failed: %w", err)
	}

	// 只发送，不等待
	if err := c.cli.SendTransaction(ctx, signedTx); err != nil {
		return nil, fmt.Errorf("send tx failed: %w", err)
	}

	return &TxResult{
		TxHash:       signedTx.Hash().Hex(),
		EstimatedGas: gasLimit,
		Nonce:        nonce,
	}, nil
}
