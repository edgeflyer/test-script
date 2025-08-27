package validator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// BinaryPath 返回可执行文件绝对路径（默认在项目根目录）
func BinaryPath() (string, error) {
	// 你也可以改成从配置或 ENV 读取
	rel := "./mobile-sdk-test"
	abs, err := filepath.Abs(rel)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// ensureExecutable 在 macOS/Linux 上尝试赋予执行权限；Windows 无需处理
func ensureExecutable(path string) error {
	_, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("binary not found: %w", err)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	// 给到 0755
	return os.Chmod(path, 0o755)
}

// ValidateOnce 运行一次 validate，并在 timeout 内返回 stdout/stderr
func ValidateOnce(ctx context.Context, validatorPrivHex string, timeout time.Duration, extraArgs ...string) (stdout string, stderr string, err error) {
	bin, err := BinaryPath()
	if err != nil {
		return "", "", err
	}
	if e := ensureExecutable(bin); e != nil {
		return "", "", e
	}

	// 组合命令参数
	args := append([]string{"validate", "--validator-private-key", validatorPrivHex}, extraArgs...)

	// 带超时的上下文
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, bin, args...)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("start validate failed: %w", err)
	}

	// 等待结束（或超时被 Context 杀掉）
	waitErr := cmd.Wait()

	// 如果是超时
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return outBuf.String(), errBuf.String(), fmt.Errorf("validate timeout after %s", timeout)
	}

	if waitErr != nil {
		return outBuf.String(), errBuf.String(), fmt.Errorf("validate exited with error: %w", waitErr)
	}
	return outBuf.String(), errBuf.String(), nil
}
