package auth

// CredentialSource 凭据来源类型
type CredentialSource string

const (
	SourceEnvVar     CredentialSource = "env_var"
	SourceConfigFile CredentialSource = "config_file"
	SourceKeyring    CredentialSource = "keyring"
	SourceBitwarden  CredentialSource = "bitwarden"
	SourceClaudeCode CredentialSource = "claude_code"
)

// Credential 表示一个凭据条目
type Credential struct {
	Key      string           // 凭据键名（如 API_KEY_NAME）
	Value    string           // 凭据值（如实际的 key）
	Source   CredentialSource // 凭据来源
	Priority int              // 数字越小优先级越高
	Provider string           // 对应的 LLM 供应商
}
