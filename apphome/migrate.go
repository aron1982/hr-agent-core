// 旧版会话数据的一次性迁移：~/.hermes-go → <HRBUDDY_HOME>。
//
// 背景：数据根收敛之前，会话库（state.db）、附件、日志分散在 ~/.hermes-go，
// 而配置、技能、记忆在 <HRBUDDY_HOME>。结果"备份/迁移要拷两个目录，漏一个就丢数据"。
// 本文件把会话数据搬到数据根下，使"拷走一个目录"等于一次完整迁移。
package apphome

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	// MigrationMarker 迁移完成标记（相对数据根）。既是幂等依据，也是审计痕迹。
	//
	// 单看"目标有没有 state.db"不够：用户手工删掉会话后，不该把旧数据再搬回来。
	MigrationMarker = ".migrated-from-legacy"

	// MigrationRootMarker 上一版数据根（整树迁移）的完成标记，与上面那个区分开：
	// 两份数据来源不同、判重条件也不同，共用一个标记会互相误判。
	MigrationRootMarker = ".migrated-from-legacy-root"

	// migrateStagePrefix 迁移暂存目录前缀（建在数据根下，与目标同分区才能原子 rename）。
	migrateStagePrefix = ".migrate-"

	// skippedOnRootMigration logs 目录在整树迁移时跳过：它是本机运行产物，
	// 且新进程启动时可能已经建了日志文件，不该被旧日志盖掉。
	skippedOnRootMigration = "logs"
)

// MigrateLegacyDataRoot 把上一版数据根**整棵树**搬进新数据根。
//
// 与 MigrateLegacyStateData 的分工：那个只搬"会话库整族 + 附件"（更早的
// ~/.hermes-go 就只有这些），这个搬技能、记忆、配置、会话在内的全部内容。
// 两者共用同一套契约：拷贝不删源、幂等、暂存后就位、失败不留半成品、失败不阻断启动。
//
// 目标已存在的文件一律保留：新根里可能已经有应用刚写下的更新数据（例如启动时
// 建的 logs/），迁移不能把更新的盖成旧的。
func MigrateLegacyDataRoot(legacyRoot, targetRoot string) (bool, error) {
	if legacyRoot == "" || targetRoot == "" {
		return false, errors.New("迁移路径为空")
	}
	if filepath.Clean(legacyRoot) == filepath.Clean(targetRoot) {
		return false, nil
	}
	if !dirExists(legacyRoot) {
		return false, nil
	}
	if fileExists(filepath.Join(targetRoot, StateDBName)) ||
		fileExists(filepath.Join(targetRoot, MigrationRootMarker)) {
		return false, nil // 新根已有会话库或已整树迁移过
	}

	if err := os.MkdirAll(targetRoot, 0o755); err != nil {
		return false, fmt.Errorf("创建数据根失败: %w", err)
	}

	stage, err := os.MkdirTemp(targetRoot, migrateStagePrefix)
	if err != nil {
		return false, fmt.Errorf("创建迁移暂存目录失败: %w", err)
	}
	defer os.RemoveAll(stage)

	// 会话库整族先单独走"拷贝期稳定性校验"：它是最怕撕裂的文件，
	// 而旧根可能仍被上一版后端或 hr-desktop 打开着
	for _, suffix := range stateDBSuffixes {
		src := filepath.Join(legacyRoot, StateDBName+suffix)
		if !fileExists(src) {
			continue
		}
		if err := copyFileStable(src, filepath.Join(stage, StateDBName+suffix)); err != nil {
			return false, fmt.Errorf("拷贝 %s 失败: %w", filepath.Base(src), err)
		}
	}

	// 其余内容整树拷贝（跳过已单独处理的会话库、logs 与两份迁移标记）
	skip := map[string]bool{
		skippedOnRootMigration: true,
		MigrationMarker:        true,
		MigrationRootMarker:    true,
	}
	for _, suffix := range stateDBSuffixes {
		skip[StateDBName+suffix] = true
	}
	if err := copyTree(legacyRoot, stage, skip, false); err != nil {
		return false, fmt.Errorf("拷贝旧数据根失败: %w", err)
	}

	// 逐个顶层条目就位；目标已有同名条目时保留目标侧（不覆盖更新的数据）
	entries, err := os.ReadDir(stage)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		dst := filepath.Join(targetRoot, e.Name())
		if fileExists(dst) || dirExists(dst) {
			continue
		}
		if err := os.Rename(filepath.Join(stage, e.Name()), dst); err != nil {
			return false, fmt.Errorf("就位 %s 失败: %w", e.Name(), err)
		}
	}

	if err := writeMarkerFile(targetRoot, MigrationRootMarker, legacyRoot); err != nil {
		// 标记写失败不影响数据可用性：下次启动会因为"新根已有 state.db"再次跳过
		return true, fmt.Errorf("写入迁移标记失败: %w", err)
	}
	return true, nil
}

// stateDBSuffixes 会话库文件族。
//
// **必须整族一起搬**：只拷主库会丢掉 -wal 里尚未检查点的事务（最隐蔽的数据丢失
// 场景）。-shm 是共享内存索引，缺失时 SQLite 会重建，但存在就一起带上更省事。
var stateDBSuffixes = []string{"", "-wal", "-shm"}

// MigrateLegacyStateData 把旧版数据目录里的会话数据拷贝到数据根。
//
// 契约：
//   - **拷贝而非移动**：旧目录保持原样，用户确认新版无问题后自行删除；
//     回退到旧版本仍能读到数据（代价是两边可能短暂分叉）
//   - **幂等**：目标已有 state.db 或已有迁移标记时直接跳过
//   - **不留半成品**：先拷进目标下的暂存目录，全部成功才 rename 就位
//   - **不改库内容**：字节级拷贝，不做 schema 变更、不升级 stateVersion
//   - **失败不阻断启动**：调用方只告警（数据仍在旧位置）
//
// 返回 true 表示本次真的迁移了数据。
func MigrateLegacyStateData(legacyDir, targetDir string) (bool, error) {
	if legacyDir == "" || targetDir == "" {
		return false, errors.New("迁移路径为空")
	}
	if filepath.Clean(legacyDir) == filepath.Clean(targetDir) {
		return false, nil
	}

	legacyDB := filepath.Join(legacyDir, StateDBName)
	if !fileExists(legacyDB) {
		return false, nil // 没有旧数据，正常路径
	}
	if fileExists(filepath.Join(targetDir, StateDBName)) || fileExists(filepath.Join(targetDir, MigrationMarker)) {
		return false, nil // 已有新数据或已迁移过
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return false, fmt.Errorf("创建数据根失败: %w", err)
	}

	// 暂存目录建在数据根内（同分区 rename 才原子），失败时整体丢弃
	stage, err := os.MkdirTemp(targetDir, migrateStagePrefix)
	if err != nil {
		return false, fmt.Errorf("创建迁移暂存目录失败: %w", err)
	}
	defer os.RemoveAll(stage)

	for _, suffix := range stateDBSuffixes {
		src := legacyDB + suffix
		if !fileExists(src) {
			continue // -shm 常常不存在
		}
		if err := copyFileStable(src, filepath.Join(stage, StateDBName+suffix)); err != nil {
			return false, fmt.Errorf("拷贝 %s 失败: %w", filepath.Base(src), err)
		}
	}
	if err := copyTree(filepath.Join(legacyDir, "attachments"), filepath.Join(stage, "attachments"), nil, true); err != nil {
		return false, fmt.Errorf("拷贝 attachments 失败: %w", err)
	}

	// 全部校验通过才就位：单文件 rename 覆盖同名文件在 Windows 上同样生效
	for _, suffix := range stateDBSuffixes {
		name := StateDBName + suffix
		if !fileExists(filepath.Join(stage, name)) {
			continue
		}
		if err := os.Rename(filepath.Join(stage, name), filepath.Join(targetDir, name)); err != nil {
			return false, fmt.Errorf("就位 %s 失败: %w", name, err)
		}
	}
	if dirExists(filepath.Join(stage, "attachments")) {
		if err := os.Rename(filepath.Join(stage, "attachments"), filepath.Join(targetDir, "attachments")); err != nil {
			return false, fmt.Errorf("就位 attachments 失败: %w", err)
		}
	}

	if err := writeMarkerFile(targetDir, MigrationMarker, legacyDir); err != nil {
		// 标记写失败不影响数据可用性：下次启动会因为"目标已有 state.db"再次跳过
		return true, fmt.Errorf("写入迁移标记失败: %w", err)
	}
	return true, nil
}

// writeMarkerFile 记录来源与时间，便于事后审计"数据从哪来"。
func writeMarkerFile(targetDir, markerName, legacyDir string) error {
	content := "from: " + legacyDir + "\nat: " + time.Now().Format(time.RFC3339) + "\n"
	return os.WriteFile(filepath.Join(targetDir, markerName), []byte(content), 0o644)
}

// copyFileStable 拷贝文件，并校验源在拷贝期间没有被改动。
//
// 为什么需要：旧库可能仍被另一个进程（例如降级运行的旧版本后端）打开并写入，
// 那会拷到一个撕裂的快照 —— 比"这次不迁、下次再试"糟得多。用 size + mtime 做
// 变动检测，不引入 SQLite 依赖也能覆盖"有进程正在写"这个真实场景。
func copyFileStable(src, dst string) error {
	before, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	after, err := os.Stat(src)
	if err != nil {
		return err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("源文件在拷贝期间被修改（可能有其他进程正在使用），本次放弃迁移")
	}

	copied, err := os.Stat(dst)
	if err != nil {
		return err
	}
	if copied.Size() != after.Size() {
		return fmt.Errorf("拷贝结果大小不一致（源 %d 字节，副本 %d 字节）", after.Size(), copied.Size())
	}
	return nil
}

// copyTree 递归拷贝目录树。
//
// 源目录不存在视为成功（附件目录这类可选目录可能从未创建）。skip 命中的名字
// 整体跳过（目录或文件）；overwrite=false 时目标已有的同名文件保留不动 ——
// 迁移不该把新根里更新的文件盖成旧的。
func copyTree(src, dst string, skip map[string]bool, overwrite bool) error {
	if !dirExists(src) {
		return nil
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if skip[e.Name()] {
			continue
		}
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())

		switch {
		case e.IsDir():
			if err := copyTree(s, d, skip, overwrite); err != nil {
				return err
			}
		case e.Type().IsRegular():
			if !overwrite && fileExists(d) {
				continue
			}
			if err := copyFile(s, d); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyFile 逐字节拷贝（权限统一 0o644：数据文件不需要可执行位）。
//
// 这两个复制辅助函数刻意留在包内而不复用 skills 包的实现：apphome 是路径约定的
// 归属者，必须保持叶子依赖，不能反过来依赖上层包。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(0o644)
}

// fileExists 判断是否为已存在的普通文件。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// dirExists 判断是否为已存在的目录。
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
