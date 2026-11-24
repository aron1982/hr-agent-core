// 随包发布的资源目录（resources/）定位。
//
// 源文件在 agent/resources（后端自有）；electron-builder extraResources 把它
// 复制进应用资源目录，让安装后的那两份文件可被查看/替换：
//
//	安装版  <可执行文件目录>/            = …/resources（后端 exe 就放在 resources/ 下）
//	          ├── skills/<slug>/SKILL.md   内置技能（播种到数据根）
//	          └── config/channels.json     出厂渠道目录（播种到数据根）
//	开发版  <可执行文件目录>/resources/  = agent/resources（exe 在 agent/ 下）
//
// 定位一律从**可执行文件**出发而不是工作目录：后端由桌面端以任意 cwd 拉起。
package apphome

import (
	"os"
	"path/filepath"
	"strings"
)

// EnvResourcesDir 覆盖资源目录的环境变量。
//
// 生产不设置（按可执行文件位置推断）。测试与被搬到别处的部署用它显式指路
// —— 测试进程的可执行文件在临时目录里，靠 exe 猜不出仓库路径。
const EnvResourcesDir = "HRBUDDY_RESOURCES_DIR"

// ResourcesDir 返回资源目录，第二个值为是否存在。
//
// 判定必须看**具体资源文件**而不是"有这个子目录"：开发场景下 exe 在 agent/
// 下，而 agent/skills 是技能的 Go 源码包目录（不含 SKILL.md），按目录名判会
// 把 agent/ 误当成资源目录，导致出厂渠道目录永远找不到。
func ResourcesDir() (string, bool) {
	if dir := strings.TrimSpace(os.Getenv(EnvResourcesDir)); dir != "" {
		if hasKnownResource(dir) {
			return dir, true
		}

		return "", false
	}

	exe, err := os.Executable()
	if err != nil {
		return "", false
	}

	exeDir := filepath.Dir(exe)

	for _, cand := range []string{
		exeDir,                             // 安装版：exe 就在 resources/ 下
		filepath.Join(exeDir, "resources"), // 开发版：agent/resources
	} {
		if hasKnownResource(cand) {
			return cand, true
		}
	}

	return "", false
}

// BundledSkillsDir 返回随包发布的内置技能目录（<资源目录>/skills，且至少含
// 一个 <slug>/SKILL.md）。
func BundledSkillsDir() (string, bool) {
	dir, ok := ResourcesDir()
	if !ok {
		return "", false
	}

	skillsDir := filepath.Join(dir, "skills")
	if !HasSkillMarkdown(skillsDir) {
		return "", false
	}

	return skillsDir, true
}

// HasSkillMarkdown 判断目录是否为有效的技能资源目录：至少存在一个
// <slug>/SKILL.md 子目录结构。
func HasSkillMarkdown(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		if _, err := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md")); err == nil {
			return true
		}
	}

	return false
}

// hasKnownResource 判断目录是否含预定义资源：内置技能或出厂配置文件。
func hasKnownResource(dir string) bool {
	if HasSkillMarkdown(filepath.Join(dir, "skills")) {
		return true
	}

	entries, err := os.ReadDir(filepath.Join(dir, "config"))
	if err != nil {
		return false
	}

	for _, e := range entries {
		if !e.IsDir() {
			return true
		}
	}

	return false
}
