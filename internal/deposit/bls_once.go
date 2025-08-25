package deposit

import (
	"sync"

	"github.com/herumi/bls-eth-go-binary/bls"
)

var blsOnce sync.Once

// EnsureBLS 在进程内只初始化一次 BLS 库
func EnsureBLS() {
	blsOnce.Do(func() {
		bls.Init(bls.BLS12_381)
		// 如需要，可开启 ETH mode（不同版本名字略有差异）
		// bls.SetETHmode(bls.EthModeLatest)
	})
}
