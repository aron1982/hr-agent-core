package core

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// MemoryManager 记忆管理器，提供键值对形式的持久记忆存储
type MemoryManager struct {
	memories map[string]string
	mu       sync.RWMutex
}

// NewMemoryManager 创建记忆管理器
func NewMemoryManager() *MemoryManager {
	return &MemoryManager{
		memories: make(map[string]string),
	}
}

// Set 设置一个记忆条目
func (m *MemoryManager) Set(key, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memories[key] = value
}

// Get 获取记忆条目
func (m *MemoryManager) Get(key string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	val, ok := m.memories[key]
	return val, ok
}

// Delete 删除记忆条目
func (m *MemoryManager) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.memories, key)
}

// Snapshot 生成记忆快照文本，按 key 排序以保证输出稳定
func (m *MemoryManager) Snapshot() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.memories) == 0 {
		return ""
	}

	keys := make([]string, 0, len(m.memories))
	for k := range m.memories {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("- %s: %s", k, m.memories[k]))
	}

	return strings.Join(parts, "\n")
}

// Len 返回记忆条目总数
func (m *MemoryManager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.memories)
}

// Clear 清空所有记忆
func (m *MemoryManager) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memories = make(map[string]string)
}
