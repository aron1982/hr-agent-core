// Package apphome 解析 hr-buddy 的数据根目录（HRBUDDY_HOME）。
//
// 解析顺序：
//  1. 环境变量 HRBUDDY_HOME
//  2. 环境变量 HERMES_HOME（旧名，仅作兼容读取；main 启动时告警提醒改名）
//  3. 平台默认目录（见 defaultHomeFor）
//  4. 兜底 ~/.hermes
//
// 平台布局：数据与渲染进程数据同处一个**应用父目录**，各自一个子目录 ——
// 这样"一个目录搬走 = 整套数据搬走"，也不会把 Chromium 缓存混进用户资产。
//
//	Windows  %LOCALAPPDATA%\hr-buddy\{data,chromium}
//	macOS    ~/Library/Application Support/hr-buddy/{data,chromium}
//	Linux    ~/.hermes（未纳入合并：没有打包产物，也没有历史包袱）
//
// data 下的内容：config.yaml / .env（模型与供应商）、skills/、memories/、
// state.db（会话库）、attachments/（图片）、logs/（后端日志）。
package apphome

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// EnvHome 指定数据根目录的环境变量名。
	EnvHome = "HRBUDDY_HOME"

	// EnvHomeLegacy 旧环境变量名，仅作兼容读取（新名优先）。
	EnvHomeLegacy = "HERMES_HOME"

	// AppDirName 应用父目录名（承载数据与渲染进程数据的共同父目录）。
	AppDirName = "hr-buddy"

	// DataDirName 数据根在 AppDirName 下的子目录名。
	DataDirName = "data"

	// ChromiumDirName Electron userData 在 AppDirName 下的子目录名
	// （desktop 主进程按同一常量设置 userData，两边保持一致）。
	ChromiumDirName = "chromium"

	// legacyDataDirName 上一版的用户级数据目录名：
	// Windows 在 %LOCALAPPDATA% 下、macOS/Linux 在 ~ 下。
	legacyDataDirName = "hermes"

	// StateDBName 会话状态库文件名。
	StateDBName = "state.db"

	// legacyStateDirName 更早的会话数据目录（相对用户主目录）。
	legacyStateDirName = ".hermes-go"

	// 自管子目录名：工具、运行时、缓存（供 uv / Python 环境复用）。
	binDirName      = "bin"
	runtimesDirName = "runtimes"
	cacheDirName    = "cache"

	// spillsDirName 工具超预算输出的落盘目录名（spill 机制）。
	spillsDirName = "spills"

	// macAppSupportDir macOS 上等价于 Windows 的 %LOCALAPPDATA%：
	// 本机应用数据，不参与 iCloud/网络账户同步。
	macAppSupportDir = "Application Support"
)

// ResolveHome 返回数据根目录。
//
// 与 Resolve 的区别：取不到时把错误交回调用方 —— 需要"落盘"的调用方（会话库）
// 据此禁用持久化，而不是退化成"." 在当前工作目录建库。
func ResolveHome() (string, error) {
	if v := strings.TrimSpace(os.Getenv(EnvHome)); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv(EnvHomeLegacy)); v != "" {
		return v, nil
	}

	if dir, err := defaultHomeFor(runtime.GOOS, os.Getenv, os.UserHomeDir); err == nil {
		return dir, nil
	}

	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", errors.New("无法解析用户主目录")
	}

	return filepath.Join(home, "."+legacyDataDirName), nil
}

// defaultHomeFor 计算平台默认数据根（<应用父目录>/data）。
//
// 抽成显式参数的函数是为了可测：runtime.GOOS 是编译期常量，测试里没法逐个
// 平台覆盖，而这三个布局恰恰是最该被钉住的东西（改错了老用户就"丢数据"）。
func defaultHomeFor(goos string, lookup func(string) string, userHome func() (string, error)) (string, error) {
	switch goos {
	case "windows":
		app := strings.TrimSpace(lookup("LOCALAPPDATA"))
		if app == "" {
			return "", errors.New("LOCALAPPDATA 未设置")
		}
		return filepath.Join(app, AppDirName, DataDirName), nil
	case "darwin":
		home, err := userHome()
		if err != nil || strings.TrimSpace(home) == "" {
			return "", errors.New("无法解析用户主目录")
		}
		return filepath.Join(home, "Library", macAppSupportDir, AppDirName, DataDirName), nil
	default:
		// 其它平台（实际就是 Linux 的 CLI 用法）保持 ~/.hermes：没有打包产物，
		// 多一套布局只会让文档与迁移逻辑再复杂一分。
		return "", errors.New("该平台没有专用默认目录")
	}
}

// Resolve 返回数据根目录；取不到时返回 "."。
//
// 保持历史行为不变：读写"可缺省"的文件（技能、记忆）在极端环境下落到当前目录，
// 而不是让整个功能不可用。
func Resolve() string {
	home, err := ResolveHome()
	if err != nil {
		return "."
	}
	return home
}

// BinDir 返回自管工具目录 <数据根>/bin，并确保其存在。
//
// 存放自动下载的单二进制工具（当前是 uv），避免污染系统 PATH。
func BinDir() (string, error) {
	return ensureSubDir(binDirName)
}

// RuntimesDir 返回运行时目录 <数据根>/runtimes，并确保其存在。
//
// 存放全局共享的语言运行时（当前是 uv 安装的 Python 解释器），
// 跨项目复用一份，重成本只付一次。
func RuntimesDir() (string, error) {
	return ensureSubDir(runtimesDirName)
}

// CacheDir 返回缓存目录 <数据根>/cache，并确保其存在。
//
// 存放包下载缓存（当前是 uv 的 UV_CACHE_DIR），跨项目硬链接复用。
func CacheDir() (string, error) {
	return ensureSubDir(cacheDirName)
}

// SpillsDir 返回 spill 落盘目录 <数据根>/spills，并确保其存在。
//
// 存放工具超预算输出的完整结果（spill 机制），供模型用 read_file 取回。
func SpillsDir() (string, error) {
	return ensureSubDir(spillsDirName)
}

// ensureSubDir 解析 <数据根>/name 并确保目录存在（0o755）。
func ensureSubDir(name string) (string, error) {
	home, err := ResolveHome()
	if err != nil {
		return "", err
	}

	dir := filepath.Join(home, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	return dir, nil
}

// StateDBPath 会话状态库路径：<数据根>/state.db。
func StateDBPath() (string, error) {
	home, err := ResolveHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, StateDBName), nil
}

// LegacyDataRoots 返回"上一版数据根"位置（整树形态：技能、记忆、配置、会话都在
// 里面），供 MigrateLegacyDataRoot 整树搬入当前数据根。
//
// 与平台默认目录一一对应：Windows 是 %LOCALAPPDATA%\hermes，macOS 是 ~/.hermes。
// 更早的会话数据位置 ~/.hermes-go 不在此列：它只有库与附件，由
// MigrateLegacyStateData 单独处理（两者各自判重，可先后执行）。
func LegacyDataRoots() []string {
	return legacyDataRootsFor(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

// legacyDataRootsFor 与 defaultHomeFor 同理抽出来以便逐平台断言。
func legacyDataRootsFor(goos string, lookup func(string) string, userHome func() (string, error)) []string {
	switch goos {
	case "windows":
		app := strings.TrimSpace(lookup("LOCALAPPDATA"))
		if app == "" {
			return nil
		}
		return []string{filepath.Join(app, legacyDataDirName)}
	case "darwin":
		home, err := userHome()
		if err != nil || strings.TrimSpace(home) == "" {
			return nil
		}
		return []string{filepath.Join(home, "."+legacyDataDirName)}
	default:
		return nil
	}
}

// LegacyStateDir 更早的会话数据目录（~/.hermes-go）：数据根收敛到单一目录之前，
// 会话库、附件、日志都写在这里。保留本函数只为一次性迁移。
func LegacyStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", errors.New("无法解析用户主目录")
	}
	return filepath.Join(home, legacyStateDirName), nil
}

// ReadDotEnvFile 解析 .env 文件（KEY=VALUE，忽略注释与空行）。
//
// 需要读 .env 的地方（模型配置、联网凭据、以及将来的其它外部配置）都必须走
// 这一份实现 —— 解析规则只有一份，避免出现"同一个键在 A 处能读到、在 B 处
// 读不到"这类极难排查的差异。
func ReadDotEnvFile(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		if key != "" {
			out[key] = val
		}
	}

	return out
}
