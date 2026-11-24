package auth

import (
	"fmt"
	"sort"
	"sync"
)

// CredentialPool 多来源凭据池，按优先级管理 API key
type CredentialPool struct {
	credentials []Credential
	mu          sync.RWMutex
}

// NewCredentialPool 创建凭据池
func NewCredentialPool() *CredentialPool {
	return &CredentialPool{
		credentials: make([]Credential, 0),
	}
}

// AddCredential 添加凭据，添加后按优先级排序
func (p *CredentialPool) AddCredential(c Credential) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.credentials = append(p.credentials, c)
	// 按优先级升序排列，数字越小优先级越高
	sort.Slice(p.credentials, func(i, j int) bool {
		return p.credentials[i].Priority < p.credentials[j].Priority
	})
}

// GetCredential 根据 provider 获取优先级最高的凭据
// 返回优先级最高（Priority 数字最小）的匹配凭据，如果没有找到则返回错误
func (p *CredentialPool) GetCredential(provider string) (*Credential, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for i := range p.credentials {
		if p.credentials[i].Provider == provider {
			return &p.credentials[i], nil
		}
	}
	return nil, fmt.Errorf("auth: 未找到 provider %q 的凭据", provider)
}

// ListCredentials 列出某个 provider 的所有凭据（按优先级排序）
func (p *CredentialPool) ListCredentials(provider string) []Credential {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]Credential, 0)
	for _, c := range p.credentials {
		if c.Provider == provider {
			result = append(result, c)
		}
	}
	return result
}

// HasCredential 检查是否有指定 provider 的凭据
func (p *CredentialPool) HasCredential(provider string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, c := range p.credentials {
		if c.Provider == provider {
			return true
		}
	}
	return false
}
