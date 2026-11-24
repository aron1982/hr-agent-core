package apphome

import (
	"os"
	"path/filepath"
	"testing"
)

func writeResourceFile(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

// TestHasKnownResourceIgnoresSourceTree 开发场景的关键坑：exe 在 agent/ 下，
// 而 agent/skills 是技能的 Go 源码包目录（没有 SKILL.md）。按"有 skills 子目录"
// 判定会把 agent/ 当成资源目录，出厂渠道目录就永远找不到。
func TestHasKnownResourceIgnoresSourceTree(t *testing.T) {
	agentDir := t.TempDir()
	writeResourceFile(t, filepath.Join(agentDir, "skills", "store.go"))

	if hasKnownResource(agentDir) {
		t.Fatal("含 Go 技能源码包的 agent 目录不应被当成资源目录")
	}

	// 光有空的 skills/ 目录也不算
	emptySkills := t.TempDir()
	if err := os.MkdirAll(filepath.Join(emptySkills, "skills"), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if hasKnownResource(emptySkills) {
		t.Fatal("空 skills 目录不应算资源")
	}
}

// TestHasKnownResourceMarkers 两类真实标记：config/ 下有文件、skills/<slug>/SKILL.md。
func TestHasKnownResourceMarkers(t *testing.T) {
	withConfig := t.TempDir()
	writeResourceFile(t, filepath.Join(withConfig, "config", "channels.json"))
	if !hasKnownResource(withConfig) {
		t.Fatal("含 config/channels.json 的目录应算资源目录")
	}

	withSkill := t.TempDir()
	writeResourceFile(t, filepath.Join(withSkill, "skills", "recruit-automation", "SKILL.md"))
	if !hasKnownResource(withSkill) {
		t.Fatal("含 skills/<slug>/SKILL.md 的目录应算资源目录")
	}
}

// TestResourcesDirEnvOverride 环境变量指路（测试/自定义部署）：
// 指到有效资源目录才认，指到无效目录则视为没有资源。
func TestResourcesDirEnvOverride(t *testing.T) {
	res := t.TempDir()
	writeResourceFile(t, filepath.Join(res, "config", "channels.json"))

	t.Setenv(EnvResourcesDir, res)

	got, ok := ResourcesDir()
	if !ok || got != res {
		t.Fatalf("应按环境变量定位资源目录，实际 %q ok=%v", got, ok)
	}

	skillsDir, ok := BundledSkillsDir()
	if ok {
		t.Fatalf("该资源目录没有 SKILL.md，不应给出技能目录: %q", skillsDir)
	}
}

// TestBundledSkillsDirEnvOverride 资源目录里有真技能时能给出技能目录。
func TestBundledSkillsDirEnvOverride(t *testing.T) {
	res := t.TempDir()
	writeResourceFile(t, filepath.Join(res, "skills", "recruit-automation", "SKILL.md"))

	t.Setenv(EnvResourcesDir, res)

	got, ok := BundledSkillsDir()
	if !ok {
		t.Fatal("应定位到内置技能目录")
	}
	if got != filepath.Join(res, "skills") {
		t.Fatalf("技能目录不符: %q", got)
	}
}

// TestResourcesDirEnvOverrideInvalid 指到无效目录时返回"没有资源"，而不是
// 回落到 exe 推断（避免半套资源混用）。
func TestResourcesDirEnvOverrideInvalid(t *testing.T) {
	t.Setenv(EnvResourcesDir, filepath.Join(t.TempDir(), "nope"))

	if _, ok := ResourcesDir(); ok {
		t.Fatal("无效的资源目录不应被接受")
	}
}
