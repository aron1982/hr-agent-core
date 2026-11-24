package infra

import (
	"os"
	"path/filepath"
	"sync"
)

// runtimeCWD 运行时工作目录（进程级单例）
var runtimeCWD struct {
	mu  sync.RWMutex
	dir string
}

func init() {
	// 初始化时为当前进程工作目录
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	runtimeCWD.dir = dir
}

// GetRuntimeCWD 获取运行时工作目录
// 返回绝对路径
func GetRuntimeCWD() string {
	runtimeCWD.mu.RLock()
	defer runtimeCWD.mu.RUnlock()

	// 如果已设置自定义目录，直接返回
	if runtimeCWD.dir != "" {
		abs, err := filepath.Abs(runtimeCWD.dir)
		if err == nil {
			return abs
		}
		return runtimeCWD.dir
	}

	// 回退到当前进程目录
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}

// SetRuntimeCWD 设置运行时工作目录
// dir 可以是相对路径或绝对路径
func SetRuntimeCWD(dir string) {
	runtimeCWD.mu.Lock()
	defer runtimeCWD.mu.Unlock()

	if dir == "" {
		return
	}

	// 展开 ~ 和规范化路径
	expanded := dir
	if len(dir) >= 2 && dir[:2] == "~"+string(os.PathSeparator) {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, dir[2:])
		}
	}

	abs, err := filepath.Abs(expanded)
	if err != nil {
		runtimeCWD.dir = expanded
		return
	}
	runtimeCWD.dir = abs
}

// ResolveRuntimeCWD 解析相对于运行时工作目录的路径
// 如果 path 是绝对路径，直接返回；否则相对于运行时目录拼接
func ResolveRuntimeCWD(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(GetRuntimeCWD(), path)
}

// ResetRuntimeCWD 重置运行时工作目录为当前进程工作目录
func ResetRuntimeCWD() {
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	runtimeCWD.mu.Lock()
	runtimeCWD.dir = dir
	runtimeCWD.mu.Unlock()
}
