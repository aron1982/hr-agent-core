// 数据根解析与旧版数据迁移的验收测试。
//
// 覆盖的契约：
//   - 环境变量优先级：HRBUDDY_HOME > HERMES_HOME（旧名）> 平台默认
//   - 会话库路径跟着数据根走（这是"拷一个目录即完整迁移"的前提）
//   - 迁移把会话库整族（state.db + -wal + -shm）与附件一起搬，且**拷贝不删源**
//   - 幂等：第二次调用、目标已有库、已写过标记，都不再搬
//   - 失败不留半成品（暂存目录被清理）
package apphome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile 写文件并建父目录。
func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

// newLegacyDir 造一个"旧版数据目录"：会话库三件套 + 一个附件。
func newLegacyDir(t *testing.T) string {
	t.Helper()

	legacy := filepath.Join(t.TempDir(), ".hermes-go")
	writeFile(t, filepath.Join(legacy, StateDBName), "SQLite format 3\x00main")
	writeFile(t, filepath.Join(legacy, StateDBName+"-wal"), "wal-pending-tx")
	writeFile(t, filepath.Join(legacy, StateDBName+"-shm"), "shm")
	writeFile(t, filepath.Join(legacy, "attachments", "sess-1", "a.png"), "png-bytes")
	writeFile(t, filepath.Join(legacy, "logs", "backend.log"), "old log")

	return legacy
}

// TestResolveHomeEnvPriority 环境变量优先级：新名 > 旧名 > 平台默认。
func TestResolveHomeEnvPriority(t *testing.T) {
	t.Setenv(EnvHome, `C:\new-home`)
	t.Setenv(EnvHomeLegacy, `C:\old-home`)

	home, err := ResolveHome()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if home != `C:\new-home` {
		t.Fatalf("新环境变量应优先，实际 %q", home)
	}

	// 只有旧名时仍兼容读取（迁移期不能让老用户"数据凭空换地方"）
	t.Setenv(EnvHome, "")
	home, err = ResolveHome()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if home != `C:\old-home` {
		t.Fatalf("旧环境变量应被兼容读取，实际 %q", home)
	}

	// 两个都没设时退回平台默认，且不等于上面任一取值
	t.Setenv(EnvHomeLegacy, "")
	home, err = ResolveHome()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if home == "" || home == "." {
		t.Fatalf("应退回平台默认目录，实际 %q", home)
	}
}

// TestStateDBPathFollowsHome 会话库路径必须跟着数据根（本次收敛的核心断言）。
func TestStateDBPathFollowsHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvHome, dir)

	path, err := StateDBPath()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := filepath.Join(dir, StateDBName)
	if path != want {
		t.Fatalf("会话库应落在数据根下：want %q, got %q", want, path)
	}
}

// TestMigrateLegacyStateData 正常迁移：整族文件 + 附件就位，源目录保留。
func TestMigrateLegacyStateData(t *testing.T) {
	legacy := newLegacyDir(t)
	target := filepath.Join(t.TempDir(), "home")

	migrated, err := MigrateLegacyStateData(legacy, target)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !migrated {
		t.Fatal("应报告已迁移")
	}

	// 会话库三件套一个都不能少：只搬主库会丢 -wal 里未检查点的事务
	for _, suffix := range stateDBSuffixes {
		name := StateDBName + suffix
		src := filepath.Join(legacy, name)
		dst := filepath.Join(target, name)
		if got, want := readFile(t, dst), readFile(t, src); got != want {
			t.Fatalf("%s 内容不一致：%q vs %q", name, got, want)
		}
	}
	if got := readFile(t, filepath.Join(target, "attachments", "sess-1", "a.png")); got != "png-bytes" {
		t.Fatalf("附件未迁移: %q", got)
	}
	// 日志不迁移（旧日志留在原处，新日志写新位置）
	if _, err := os.Stat(filepath.Join(target, "logs", "backend.log")); err == nil {
		t.Fatal("logs 不应被迁移")
	}
	// 拷贝而非移动：源目录必须原样保留，否则回退旧版本就没数据了
	if _, err := os.Stat(filepath.Join(legacy, StateDBName)); err != nil {
		t.Fatalf("源文件不应被删除: %v", err)
	}
	if got := readFile(t, filepath.Join(target, MigrationMarker)); !strings.Contains(got, legacy) {
		t.Fatalf("标记应记录来源路径: %q", got)
	}
	assertNoStaging(t, target)
}

// homeFn 返回固定主目录的 userHome 替身。
func homeFn(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

// lookupFn 把 map 包成 os.Getenv 形状。
func lookupFn(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// TestDefaultHomeForPlatforms 逐平台断言数据根布局。
//
// runtime.GOOS 是编译期常量，测试里没法真的切平台，所以直接喂参数：
// 这三个布局恰恰是最不能改错的东西（改错了老用户等于"丢数据"）。
func TestDefaultHomeForPlatforms(t *testing.T) {
	const unixHome = "/Users/me"
	const winLocal = `C:\Users\me\AppData\Local`

	cases := []struct {
		name    string
		goos    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{
			name: "windows 收进 %LOCALAPPDATA%\\hr-buddy\\data",
			goos: "windows",
			env:  map[string]string{"LOCALAPPDATA": winLocal},
			want: filepath.Join(winLocal, AppDirName, DataDirName),
		},
		{
			name:    "windows 缺 LOCALAPPDATA 时不可用（不退化成当前目录）",
			goos:    "windows",
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name: "macOS 收进 ~/Library/Application Support/hr-buddy/data",
			goos: "darwin",
			want: filepath.Join(unixHome, "Library", "Application Support", AppDirName, DataDirName),
		},
		{
			name:    "其它平台不参与合并（退回 ~/.hermes）",
			goos:    "linux",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := defaultHomeFor(tc.goos, lookupFn(tc.env), homeFn(unixHome))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应返回错误，实际 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}

// TestLegacyDataRootsForPlatforms 迁移来源要与平台默认目录一一对应。
func TestLegacyDataRootsForPlatforms(t *testing.T) {
	const unixHome = "/Users/me"
	const winLocal = `C:\Users\me\AppData\Local`

	cases := []struct {
		name string
		goos string
		env  map[string]string
		want []string
	}{
		{"windows", "windows", map[string]string{"LOCALAPPDATA": winLocal}, []string{filepath.Join(winLocal, "hermes")}},
		{"windows 缺 env", "windows", map[string]string{}, nil},
		{"macOS", "darwin", nil, []string{filepath.Join(unixHome, ".hermes")}},
		{"其它平台", "linux", nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := legacyDataRootsFor(tc.goos, lookupFn(tc.env), homeFn(unixHome))
			if len(got) != len(tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("want %v, got %v", tc.want, got)
				}
			}
		})
	}
}

// TestMigrateLegacyDataRoot 整树迁移：技能/记忆/配置/会话都过来，logs 不来，
// 目标已有的更新文件不被覆盖，旧根保留。
func TestMigrateLegacyDataRoot(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "hermes")
	writeFile(t, filepath.Join(legacy, StateDBName), "db-main")
	writeFile(t, filepath.Join(legacy, StateDBName+"-wal"), "db-wal-pending")
	writeFile(t, filepath.Join(legacy, "skills", "demo", "SKILL.md"), "old skill")
	writeFile(t, filepath.Join(legacy, "memories", "MEMORY.md"), "old memory")
	writeFile(t, filepath.Join(legacy, "config.yaml"), "model:\n  model: old")
	writeFile(t, filepath.Join(legacy, "logs", "backend.log"), "old log")
	writeFile(t, filepath.Join(legacy, MigrationMarker), "from: even older")

	target := filepath.Join(t.TempDir(), "hr-buddy", "data")
	// 模拟启动早期就已经写下的内容：新日志 + 刚播种的同名技能
	writeFile(t, filepath.Join(target, "logs", "backend.log"), "new log")
	writeFile(t, filepath.Join(target, "skills", "demo", "SKILL.md"), "newer skill")

	migrated, err := MigrateLegacyDataRoot(legacy, target)
	if err != nil || !migrated {
		t.Fatalf("应迁移: migrated=%v err=%v", migrated, err)
	}

	// 会话库整族都要过来（-wal 里可能有未检查点的事务）
	for _, suffix := range []string{"", "-wal"} {
		name := StateDBName + suffix
		if got, want := readFile(t, filepath.Join(target, name)), readFile(t, filepath.Join(legacy, name)); got != want {
			t.Fatalf("%s 内容不一致：%q vs %q", name, got, want)
		}
	}
	if got := readFile(t, filepath.Join(target, "memories", "MEMORY.md")); got != "old memory" {
		t.Fatalf("记忆未迁移: %q", got)
	}
	if got := readFile(t, filepath.Join(target, "config.yaml")); !strings.Contains(got, "old") {
		t.Fatalf("配置未迁移: %q", got)
	}
	// logs 跳过：不该被旧日志盖掉
	if got := readFile(t, filepath.Join(target, "logs", "backend.log")); got != "new log" {
		t.Fatalf("logs 应跳过，实际 %q", got)
	}
	// 目标已有的同名文件保留（迁移不能把更新的盖成旧的）
	if got := readFile(t, filepath.Join(target, "skills", "demo", "SKILL.md")); got != "newer skill" {
		t.Fatalf("目标已有文件应保留，实际 %q", got)
	}
	// 旧根的迁移标记不该被带进来，避免"已迁移"误判
	if fileExists(filepath.Join(target, MigrationMarker)) {
		t.Fatal("不应把旧根的迁移标记带进新根")
	}
	if got := readFile(t, filepath.Join(target, MigrationRootMarker)); !strings.Contains(got, legacy) {
		t.Fatalf("整树迁移标记应记录来源: %q", got)
	}
	// 拷贝而非移动
	if _, err := os.Stat(filepath.Join(legacy, StateDBName)); err != nil {
		t.Fatalf("源不应被删除: %v", err)
	}
	assertNoStaging(t, target)

	// 幂等
	if migrated, err := MigrateLegacyDataRoot(legacy, target); err != nil || migrated {
		t.Fatalf("重复迁移应跳过: migrated=%v err=%v", migrated, err)
	}
}

// TestMigrateLegacyDataRootSkipCases 旧根不存在 / 路径相同 / 已迁移过。
func TestMigrateLegacyDataRootSkipCases(t *testing.T) {
	target := filepath.Join(t.TempDir(), "hr-buddy", "data")

	if migrated, err := MigrateLegacyDataRoot(filepath.Join(t.TempDir(), "nope"), target); err != nil || migrated {
		t.Fatalf("旧根不存在应跳过: migrated=%v err=%v", migrated, err)
	}

	legacy := filepath.Join(t.TempDir(), "hermes")
	writeFile(t, filepath.Join(legacy, StateDBName), "db")
	if migrated, err := MigrateLegacyDataRoot(legacy, legacy); err != nil || migrated {
		t.Fatalf("源与目标相同应跳过: migrated=%v err=%v", migrated, err)
	}
	if _, err := MigrateLegacyDataRoot("", target); err == nil {
		t.Fatal("空路径应报错")
	}
}

// TestMigrateLegacyStateDataIdempotent 幂等：重复调用、目标已有库、已迁移过都不再搬。
func TestMigrateLegacyStateDataIdempotent(t *testing.T) {
	legacy := newLegacyDir(t)
	target := filepath.Join(t.TempDir(), "home")

	if _, err := MigrateLegacyStateData(legacy, target); err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	// 第二次：目标已有库
	migrated, err := MigrateLegacyStateData(legacy, target)
	if err != nil || migrated {
		t.Fatalf("重复迁移应跳过: migrated=%v err=%v", migrated, err)
	}

	// 目标库被删但标记仍在：不该把旧数据"复活"（用户可能是有意清空会话）
	if err := os.Remove(filepath.Join(target, StateDBName)); err != nil {
		t.Fatalf("删除目标库失败: %v", err)
	}
	migrated, err = MigrateLegacyStateData(legacy, target)
	if err != nil || migrated {
		t.Fatalf("有标记时应跳过: migrated=%v err=%v", migrated, err)
	}
}

// TestMigrateLegacyStateDataSkipCases 无旧数据 / 路径相同 / 路径为空。
func TestMigrateLegacyStateDataSkipCases(t *testing.T) {
	target := filepath.Join(t.TempDir(), "home")

	empty := filepath.Join(t.TempDir(), "empty-legacy")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if migrated, err := MigrateLegacyStateData(empty, target); err != nil || migrated {
		t.Fatalf("无旧库应跳过: migrated=%v err=%v", migrated, err)
	}

	legacy := newLegacyDir(t)
	if migrated, err := MigrateLegacyStateData(legacy, legacy); err != nil || migrated {
		t.Fatalf("源与目标相同应跳过: migrated=%v err=%v", migrated, err)
	}
	if _, err := MigrateLegacyStateData("", target); err == nil {
		t.Fatal("空路径应报错")
	}
}

// TestMigrateLegacyStateDataDBOnly 只有主库（没有 -wal/-shm/附件）也要能迁。
func TestMigrateLegacyStateDataDBOnly(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), ".hermes-go")
	writeFile(t, filepath.Join(legacy, StateDBName), "only-main")
	target := filepath.Join(t.TempDir(), "home")

	migrated, err := MigrateLegacyStateData(legacy, target)
	if err != nil || !migrated {
		t.Fatalf("应迁移: migrated=%v err=%v", migrated, err)
	}
	if got := readFile(t, filepath.Join(target, StateDBName)); got != "only-main" {
		t.Fatalf("主库未迁移: %q", got)
	}
	if _, err := os.Stat(filepath.Join(target, StateDBName+"-wal")); err == nil {
		t.Fatal("源里没有 -wal 就不该造一个出来")
	}
	if _, err := os.Stat(filepath.Join(target, "attachments")); err == nil {
		t.Fatal("源里没有附件目录就不该造一个出来")
	}
	assertNoStaging(t, target)
}

// readFile 读文件，失败即 Fatal。
func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(data)
}

// assertNoStaging 断言数据根下没有残留的迁移暂存目录（"不留半成品"）。
func assertNoStaging(t *testing.T, target string) {
	t.Helper()

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("读取数据根失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), migrateStagePrefix) {
			t.Fatalf("残留暂存目录: %s", e.Name())
		}
	}
}
