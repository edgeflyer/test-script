package deposit

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/herumi/bls-eth-go-binary/bls"
)

/*
提供能力：
1) ComputeDepositSignatureAndRoot：计算 BLS 签名(96B) 与 deposit_data_root(32B)
2) ComputeWithdrawalCredentialsFromEth1：从执行层地址生成 withdrawal_credentials

实现说明（最小 SSZ HTR）：
- bytesN：按 32 字节分块，尾块右侧零填充，merkleize
- uint64：小端写入 8 字节，放在 32 字节 chunk 前 8 字节，其余补 0
- Container：把各字段的 32B 根顺序拼接成叶子做 merkleize
- signing_root = HTR(SigningData{ObjectRoot, Domain})
- DOMAIN_DEPOSIT = 0x03000000 + 28*0x00
*/

// ---------------- Domain 常量 ----------------

// 后面需要手动修改domain！！！！！
var DOMAIN_DEPOSIT = func() [32]byte {
	var d [32]byte
	d[0] = 0x03 // 0x03000000 + 28*0x00
	return d
}()

// ---------------- SSZ 基础工具 ----------------

var zeroChunk = [32]byte{}

// 将任意 data 按 32 字节切片，不足最后一块右侧补零
func chunkify(data []byte) [][32]byte {
	if len(data) == 0 {
		return [][32]byte{{}} // 至少一块零块，避免空容器
	}
	n := (len(data) + 31) / 32
	out := make([][32]byte, n)
	for i := 0; i < n; i++ {
		start := i * 32
		end := start + 32
		if end > len(data) {
			end = len(data)
		}
		copy(out[i][:], data[start:end])
	}
	return out
}

// Merkleize：对若干 32 字节块做二叉 Merkle，补到 2^k 叶子
func merkleize(leaves [][32]byte) [32]byte {
	if len(leaves) == 0 {
		return zeroChunk
	}
	// 扩展到 2^k 叶子
	size := 1
	for size < len(leaves) {
		size <<= 1
	}
	nodes := make([][32]byte, size)
	copy(nodes, leaves)
	for i := len(leaves); i < size; i++ {
		nodes[i] = zeroChunk
	}
	// 自底向上
	for width := size; width > 1; width >>= 1 {
		next := make([][32]byte, width/2)
		for i := 0; i < width; i += 2 {
			h := sha256.New()
			h.Write(nodes[i][:])
			h.Write(nodes[i+1][:])
			copy(next[i/2][:], h.Sum(nil))
		}
		nodes = next
	}
	return nodes[0]
}

// SSZ: hash_tree_root(bytesN)
func htrBytesN(b []byte) [32]byte {
	chunks := chunkify(b)
	return merkleize(chunks)
}

// SSZ: hash_tree_root(uint64) 基本类型：LE 放前 8 字节，补成 32 字节块
func htrUint64LE(u uint64) [32]byte {
	var chunk [32]byte
	binary.LittleEndian.PutUint64(chunk[:8], u)
	return chunk
}

// SSZ: hash_tree_root(Container{fields...})
func htrContainer(fields ...[32]byte) [32]byte {
	if len(fields) == 0 {
		return zeroChunk
	}
	return merkleize(fields)
}

// ---------------- 类型对应到 SSZ ----------------

// DepositMessage = {pubkey: bytes48, withdrawal_credentials: bytes32, amount: uint64}
func htrDepositMessage(pubkey48 []byte, wc32 []byte, amountGwei uint64) ([32]byte, error) {
	if len(pubkey48) != 48 {
		return [32]byte{}, fmt.Errorf("pubkey must be 48 bytes, got %d", len(pubkey48))
	}
	if len(wc32) != 32 {
		return [32]byte{}, fmt.Errorf("withdrawal_credentials must be 32 bytes, got %d", len(wc32))
	}
	pubkeyRoot := htrBytesN(pubkey48)  // 48B
	wcRoot := htrBytesN(wc32)          // 32B
	amtRoot := htrUint64LE(amountGwei) // 8B->32B
	return htrContainer(pubkeyRoot, wcRoot, amtRoot), nil
}

// SigningData = {object_root: bytes32, domain: bytes32}
func htrSigningData(objectRoot [32]byte, domain [32]byte) [32]byte {
	return htrContainer(objectRoot, domain)
}

// DepositData = {pubkey: bytes48, withdrawal_credentials: bytes32, amount: uint64, signature: bytes96}
func htrDepositData(pubkey48 []byte, wc32 []byte, amountGwei uint64, sig96 []byte) ([32]byte, error) {
	if len(pubkey48) != 48 {
		return [32]byte{}, fmt.Errorf("pubkey must be 48 bytes, got %d", len(pubkey48))
	}
	if len(wc32) != 32 {
		return [32]byte{}, fmt.Errorf("withdrawal_credentials must be 32 bytes, got %d", len(wc32))
	}
	if len(sig96) != 96 {
		return [32]byte{}, fmt.Errorf("signature must be 96 bytes, got %d", len(sig96))
	}
	pubkeyRoot := htrBytesN(pubkey48)  // 48B
	wcRoot := htrBytesN(wc32)          // 32B
	amtRoot := htrUint64LE(amountGwei) // 8B->32B
	sigRoot := htrBytesN(sig96)        // 96B
	return htrContainer(pubkeyRoot, wcRoot, amtRoot, sigRoot), nil
}

// ---------------- 对外工具函数 ----------------

// 严格长度 hex 解码
func decodeExactHex(s string, want int) ([]byte, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(s), "0x")
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("hex decode failed: %w", err)
	}
	if len(b) != want {
		return nil, fmt.Errorf("invalid length %d want %d", len(b), want)
	}
	return b, nil
}

// 计算：BLS 签名(96B hex) + deposit_data_root(32B hex)
func ComputeDepositSignatureAndRoot(
	pubkeyHex string,
	withdrawalCredHex string,
	amountGwei uint64,
	blsSkHex string,
) (signatureHex string, depositDataRootHex string, err error) {

	EnsureBLS()
	// 1) 解析 hex
	pubkey, err := decodeExactHex(pubkeyHex, 48)
	if err != nil {
		return "", "", fmt.Errorf("pubkey: %w", err)
	}
	wc, err := decodeExactHex(withdrawalCredHex, 32)
	if err != nil {
		return "", "", fmt.Errorf("withdrawal_credentials: %w", err)
	}

	// 2) message_root
	msgRoot, err := htrDepositMessage(pubkey, wc, amountGwei)
	if err != nil {
		return "", "", err
	}

	// 3) signing_root = HTR(SigningData{msgRoot, DOMAIN_DEPOSIT})
	signingRoot := htrSigningData(msgRoot, DOMAIN_DEPOSIT)

	// 4) BLS 签名 (G2，96B)
	// bls.Init(bls.BLS12_381)
	var sk bls.SecretKey
	if err := sk.SetHexString(strings.TrimPrefix(blsSkHex, "0x")); err != nil {
		return "", "", fmt.Errorf("set BLS secret key failed: %w", err)
	}
	sig := sk.SignByte(signingRoot[:])
	sigBytes := sig.Serialize()
	if len(sigBytes) != 96 {
		return "", "", errors.New("unexpected bls signature length")
	}
	signatureHex = "0x" + hex.EncodeToString(sigBytes)

	// 5) deposit_data_root = HTR(DepositData{..., signature})
	ddRoot, err := htrDepositData(pubkey, wc, amountGwei, sigBytes)
	if err != nil {
		return "", "", err
	}
	depositDataRootHex = "0x" + hex.EncodeToString(ddRoot[:])
	return
}

// 从执行层地址(20B)构造 ETH1 类型的 withdrawal_credentials：
// wc = 0x01 || 11*0x00 || sha256(address)[12:]
func ComputeWithdrawalCredentialsFromEth1(executionAddressHex string) (string, error) {
	addrBytes, err := hex.DecodeString(strings.TrimPrefix(executionAddressHex, "0x"))
	if err != nil {
		return "", fmt.Errorf("decode address hex failed: %w", err)
	}
	if len(addrBytes) != 20 {
		return "", fmt.Errorf("execution address must be 20 bytes")
	}
	// hash := sha256.Sum256(addrBytes)
	var wc [32]byte
	wc[0] = 0x01
	copy(wc[12:], addrBytes)
	return "0x" + hex.EncodeToString(wc[:]), nil
}

// 根据 已给定的 signature(96B hex) 计算 deposit_data_root（32B hex）
func ComputeDepositDataRoot(pubkeyHex string, withdrawalCredHex string, amountGwei uint64, signatureHex string) (string, error) {
	pubkey, err := decodeExactHex(pubkeyHex, 48)
	if err != nil {
		return "", fmt.Errorf("pubkey: %w", err)
	}
	wc, err := decodeExactHex(withdrawalCredHex, 32)
	if err != nil {
		return "", fmt.Errorf("withdrawal_credentials: %w", err)
	}
	sig, err := decodeExactHex(signatureHex, 96)
	if err != nil {
		return "", fmt.Errorf("signature: %w", err)
	}

	ddRoot, err := htrDepositData(pubkey, wc, amountGwei, sig)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(ddRoot[:]), nil
}
