package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// CredentialStore 凭据持久化存储（基于 JSON 文件）
type CredentialStore struct {
	filePath string
	mu       sync.RWMutex
}

// NewCredentialStore 创建持久化存储
// filePath: 凭据存储文件的路径，如 "/path/to/credentials.json"
func NewCredentialStore(filePath string) *CredentialStore {
	return &CredentialStore{
		filePath: filePath,
	}
}

// Save 保存凭据列表到文件
func (s *CredentialStore) Save(creds []Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 确保目录存在
	dir := filepath.Dir(s.filePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("auth: 创建凭据存储目录失败: %w", err)
	}

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("auth: 序列化凭据失败: %w", err)
	}

	if err := os.WriteFile(s.filePath, data, 0600); err != nil {
		return fmt.Errorf("auth: 写入凭据文件失败: %w", err)
	}

	return nil
}

// Load 从文件加载凭据列表
// 如果文件不存在，返回空切片（不报错）
func (s *CredentialStore) Load() ([]Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return make([]Credential, 0), nil
		}
		return nil, fmt.Errorf("auth: 读取凭据文件失败: %w", err)
	}

	var creds []Credential
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("auth: 解析凭据文件失败: %w", err)
	}

	return creds, nil
}
