package apphome

import (
	"path/filepath"
)

// DotEnvName 数据根下环境变量文件的文件名。
const DotEnvName = ".env"

// DotEnvPath 返回 <数据根>/.env 的路径。
//
// 该文件由**外部**写入（desktop 写模型 key、用户手工编辑），本产品只读。
func DotEnvPath() string {
	return filepath.Join(Resolve(), DotEnvName)
}

// ReadDotEnv 读取并解析 <数据根>/.env。
func ReadDotEnv() map[string]string {
	return ReadDotEnvFile(DotEnvPath())
}
