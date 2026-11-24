package apphome

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadDotEnvFile 验证 .env 解析：KEY=VALUE、注释、引号值、坏行。
//
// 覆盖点来自真实写入方的格式约定（见 dotenv.go 顶部）：刻意不支持
// `export KEY=VAL`、变量插值、多行值，因此这里只断言受支持的那部分。
func TestReadDotEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `# comment
DEEPSEEK_APIKEY=sk-abc

OPENAI_API_KEY="sk-quoted"
SINGLE='sk-single'
BAD_LINE_NO_EQUALS
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 .env 失败: %v", err)
	}

	env := ReadDotEnvFile(path)
	if env["DEEPSEEK_APIKEY"] != "sk-abc" {
		t.Fatalf("DEEPSEEK_APIKEY 解析错误: %q", env["DEEPSEEK_APIKEY"])
	}
	if env["OPENAI_API_KEY"] != "sk-quoted" {
		t.Fatalf("双引号未剥离: %q", env["OPENAI_API_KEY"])
	}
	if env["SINGLE"] != "sk-single" {
		t.Fatalf("单引号未剥离: %q", env["SINGLE"])
	}
	if len(env) != 3 {
		t.Fatalf("应解析出 3 个键，实际 %d: %v", len(env), env)
	}
}

// TestReadDotEnvFileMissing 文件不存在返回空 map（用户还没配过任何 key 的正常情况）。
func TestReadDotEnvFileMissing(t *testing.T) {
	env := ReadDotEnvFile(filepath.Join(t.TempDir(), "nope.env"))
	if len(env) != 0 {
		t.Fatalf("缺失文件应返回空 map，实际 %v", env)
	}
}
