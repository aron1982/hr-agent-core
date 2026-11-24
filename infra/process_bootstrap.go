package infra

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// bootstrapOnce 确保引导只执行一次
var bootstrapOnce sync.Once

// Bootstrap 执行进程引导
// 包括 stdio 安全安装、信号处理等
// 多次调用安全（只执行一次）
func Bootstrap() error {
	var err error
	bootstrapOnce.Do(func() {
		if e := InstallSafeStdio(); e != nil {
			err = e
		}
	})
	return err
}

// InstallSafeStdio 安装安全的 stdio 处理
// 安装 SIGPIPE 忽略处理器，防止管道断开导致进程崩溃
// 返回安全的 Writer 供调用方使用（不直接替换 os.Stdout）
func InstallSafeStdio() error {
	installSignalHandlers()
	return nil
}

// installSignalHandlers 安装信号处理器
// 在 POSIX 系统上忽略 SIGPIPE（管道断开）
func installSignalHandlers() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGPIPE)

	go func() {
		for range sigCh {
			// 忽略 SIGPIPE：管道断开时静默处理
		}
	}()
}

// ResetBootstrap 重置引导状态（主要用于测试）
func ResetBootstrap() {
	bootstrapOnce = sync.Once{}
}
