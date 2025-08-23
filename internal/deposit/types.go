package deposit

import (
	"errors"
	"math/big"
)

var (
	ErrInvalidPubkeyLen = errors.New("invalid pubkey: expect 48 or 96 bytes (BLS pubkey usually 48)")
	ErrInvalidWCLen     = errors.New("invalid withdrawal_credentials: expect 32 bytes")
	ErrInvalidSigLen    = errors.New("invalid signature: expect 96 bytes (BLS signature)")
	ErrInvalidRootLen   = errors.New("invalid deposit_data_root: expect 32 bytes")
)

type DepositParams struct {
	// 合约地址（必填）
	Contract string

	// 发送者私钥 (0x 开头十六进制)（必填）
	PrivateKeyHex string

	// RPC 端点（必填）
	RPC string

	// 以下四个字段与ETH2 DepositData一致
	PubkeyHex    string // BLS 公钥，通常 48 字节（0x 前缀的hex）
	WCHex        string // withdrawal_credentials，32字节
	SignatureHex string // BLS 签名，96字节
	RootHex      string // deposit_data_root，32字节

	// 质押转账金额（wei）。主网固定 32 ETH，这里保留自定义以兼容本地链/测试
	AmountWei *big.Int

	// 可选：nonce（为 -1 表示自动读取）
	Nonce int64

	// 可选：自定义 gas 限制（0 表示自动估算）
	GasLimit uint64

	// 可选：EIP-1559 参数（如为 nil 则自动建议）
	MaxPriorityFeePerGas *big.Int
	MaxFeePerGas         *big.Int
}

type TxResult struct {
	TxHash       string
	UsedGas      uint64
	Nonce        uint64
	EstimatedGas uint64
}
