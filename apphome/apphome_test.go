// 自管子目录路径函数的验收测试。
//
// 契约：BinDir / RuntimesDir / CacheDir 都跟随 HRBUDDY_HOME，
// 且调用即确保目录存在（调用方后续要往里面写 uv 二进制、解释器、缓存）。
package apphome

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSelfManagedDirs 三个自管子目录都落在数据根之下且被自动创建。
func TestSelfManagedDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, home)

	cases := []struct {
		name string
		fn   func() (string, error)
		sub  string
	}{
		{"BinDir", BinDir, binDirName},
		{"RuntimesDir", RuntimesDir, runtimesDirName},
		{"CacheDir", CacheDir, cacheDirName},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.fn()
			if err != nil {
				t.Fatalf("%s() 返回错误: %v", c.name, err)
			}

			want := filepath.Join(home, c.sub)
			if got != want {
				t.Fatalf("%s() = %q，期望 %q", c.name, got, want)
			}

			info, err := os.Stat(got)
			if err != nil {
				t.Fatalf("目录未创建: %v", err)
			}
			if !info.IsDir() {
				t.Fatalf("%s 不是目录", got)
			}
		})
	}
}

// TestSelfManagedDirsIdempotent 目录已存在时重复调用不报错（幂等）。
func TestSelfManagedDirsIdempotent(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())

	for i := 0; i < 2; i++ {
		if _, err := BinDir(); err != nil {
			t.Fatalf("第 %d 次 BinDir() 失败: %v", i+1, err)
		}
		if _, err := RuntimesDir(); err != nil {
			t.Fatalf("第 %d 次 RuntimesDir() 失败: %v", i+1, err)
		}
		if _, err := CacheDir(); err != nil {
			t.Fatalf("第 %d 次 CacheDir() 失败: %v", i+1, err)
		}
	}
}
