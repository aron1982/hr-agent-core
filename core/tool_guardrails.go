package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// GuardrailDecision guardrail 决策枚举
type GuardrailDecision int

const (
	GuardrailAllow   GuardrailDecision = iota // 允许执行
	GuardrailDeny                             // 拒绝执行
	GuardrailAskUser                          // 需要用户确认
)

// String 返回决策的人类可读名称
func (d GuardrailDecision) String() string {
	switch d {
	case GuardrailAllow:
		return "allow"
	case GuardrailDeny:
		return "deny"
	case GuardrailAskUser:
		return "ask_user"
	default:
		return "unknown"
	}
}

// GuardrailConfig guardrail 配置
type GuardrailConfig struct {
	DenyCommands     []string // 完全禁止的工具名（精确匹配）
	DenyArgs         []string // 参数中出现即拒绝的危险串（子串匹配）
	AskCommands      []string // 需要用户确认的命令名/前缀列表
	MaxCommandLength int      // 命令参数最大长度（超过则拒绝）
}

// DefaultGuardrailConfig 返回默认的 guardrail 配置
func DefaultGuardrailConfig() GuardrailConfig {
	return GuardrailConfig{
		// 工具名按精确匹配。此前按子串包含判断，deny 里的 "format"
		// 会误伤任何名字里带 format 的工具。
		DenyCommands: []string{
			"format",
			"mkfs",
			"shutdown",
			"reboot",
		},
		// 参数按子串匹配（终端命令串），因此只放不会误伤完整危险串：
		// 裸 "format" 会把 "?format=json" 这类参数一起拒掉，故写 "format "。
		DenyArgs: []string{
			"rm -rf /",
			"format ",
			"mkfs",
			"shutdown",
			"reboot",
		},
		AskCommands: []string{
			"rm -rf",
			"sudo",
			"chmod 777",
		},
		MaxCommandLength: 10000,
	}
}

// CheckToolSafety 检查工具调用是否安全
//
// 使用 GuardrailConfig 中的 deny/ask 列表检查工具名和参数。
// 返回 GuardrailDecision 指示是否允许执行。
func CheckToolSafety(cfg GuardrailConfig, toolName string, args string) GuardrailDecision {
	// 参数长度检查
	if cfg.MaxCommandLength > 0 && len(args) > cfg.MaxCommandLength {
		return GuardrailDeny
	}

	argsLower := strings.ToLower(args)
	nameLower := strings.ToLower(toolName)

	// 检查 deny 列表（工具名）：精确匹配，不能用子串包含，
	// 否则 deny 里的 "format" 会误伤任何名字含 format 的工具。
	for _, denied := range cfg.DenyCommands {
		if nameLower == strings.ToLower(denied) {
			return GuardrailDeny
		}
	}

	// 检查 deny 列表（参数内容）：危险命令串按子串匹配
	for _, denied := range cfg.DenyArgs {
		if strings.Contains(argsLower, strings.ToLower(denied)) {
			return GuardrailDeny
		}
	}

	// 检查 ask 列表（工具名或参数）
	for _, ask := range cfg.AskCommands {
		askLower := strings.ToLower(ask)
		if strings.Contains(nameLower, askLower) || strings.Contains(argsLower, askLower) {
			return GuardrailAskUser
		}
	}

	return GuardrailAllow
}

// ---------------------------------------------------------------------------
// 工具调用护栏（回合内循环控制）
// ---------------------------------------------------------------------------

// ToolGuardAction 护栏对一次工具调用给出的处置。
type ToolGuardAction int

const (
	// ToolGuardAllow 正常放行。
	ToolGuardAllow ToolGuardAction = iota
	// ToolGuardWarn 放行，但在结果里追加提示（模型可据此自我纠正）。
	ToolGuardWarn
	// ToolGuardBlock 不执行，直接把原因作为工具结果交回模型。
	ToolGuardBlock
	// ToolGuardHalt 结束当前回合（同一工具反复失败 / 连续无进展）。
	ToolGuardHalt
)

// ToolGuardrailConfig 工具调用护栏阈值。
//
// 默认**只告警不熔断**（对齐 hermes-agent 的 ToolCallGuardrailConfig：
// warnings_enabled=true / hard_stop_enabled=false）—— 先让模型自己纠正，
// 避免一次误判就把回合掐死。例外是 IdenticalCallCap：完全相同的调用重复
// 太多次属于死循环，无论开关都阻断。
type ToolGuardrailConfig struct {
	WarningsEnabled bool // 是否产出告警提示
	HardStopEnabled bool // 是否真的熔断（false = 达到阈值也只告警）

	ExactFailureWarnAfter    int // 同工具同参数连续失败：告警阈值
	ExactFailureBlockAfter   int // 同工具同参数连续失败：阻断阈值
	SameToolFailureWarnAfter int // 同一工具累计失败：告警阈值
	SameToolFailureHaltAfter int // 同一工具累计失败：熔断阈值
	NoProgressWarnAfter      int // 连续相同结果：告警阈值
	NoProgressBlockAfter     int // 连续相同结果：熔断阈值
	IdenticalCallCap         int // 同工具同参数累计调用上限（死循环保护）
}

// DefaultToolGuardrailConfig 默认阈值，数值取自 hermes-agent。
func DefaultToolGuardrailConfig() ToolGuardrailConfig {
	return ToolGuardrailConfig{
		WarningsEnabled: true,
		HardStopEnabled: false,

		ExactFailureWarnAfter:    2,
		ExactFailureBlockAfter:   5,
		SameToolFailureWarnAfter: 3,
		SameToolFailureHaltAfter: 8,
		NoProgressWarnAfter:      2,
		NoProgressBlockAfter:     5,
		IdenticalCallCap:         20,
	}
}

// ToolCallGuardrail 回合内的工具调用护栏：检测重复失败与无进展。
//
// 计数**每回合清零**（ResetForTurn），与 hermes-agent 的 reset_for_turn
// 一致 —— 跨回合累计会把"用户本来就在反复试"误判为死循环。
type ToolCallGuardrail struct {
	cfg ToolGuardrailConfig

	mu             sync.Mutex
	identicalCalls map[string]int // 调用指纹 → 累计次数
	identicalFails map[string]int // 调用指纹 → 连续失败次数
	toolFails      map[string]int // 工具名 → 累计失败次数
	lastResultHash string         // 上一次结果的指纹（无进展检测）
	noProgress     int            // 连续相同结果的次数
}

// NewToolCallGuardrail 创建护栏控制器。
func NewToolCallGuardrail(cfg ToolGuardrailConfig) *ToolCallGuardrail {
	return &ToolCallGuardrail{
		cfg:            cfg,
		identicalCalls: make(map[string]int),
		identicalFails: make(map[string]int),
		toolFails:      make(map[string]int),
	}
}

// ResetForTurn 回合开始时清零全部计数。
func (g *ToolCallGuardrail) ResetForTurn() {
	if g == nil {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.identicalCalls = make(map[string]int)
	g.identicalFails = make(map[string]int)
	g.toolFails = make(map[string]int)
	g.lastResultHash = ""
	g.noProgress = 0
}

// BeforeCall 执行前闸门。
//
// 只拦两类：完全相同调用达到上限（死循环），以及（熔断开启时）同一调用
// 连续失败过多。其余一律放行，交给 AfterCall 观察。
func (g *ToolCallGuardrail) BeforeCall(name, args string) (ToolGuardAction, string) {
	if g == nil {
		return ToolGuardAllow, ""
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	fp := toolCallFingerprint(name, args)
	if g.cfg.IdenticalCallCap > 0 && g.identicalCalls[fp] >= g.cfg.IdenticalCallCap {
		return ToolGuardBlock, fmt.Sprintf(
			"同一工具调用（%s）已重复 %d 次，判定为死循环，已阻断", name, g.identicalCalls[fp])
	}

	if g.cfg.HardStopEnabled && g.cfg.ExactFailureBlockAfter > 0 &&
		g.identicalFails[fp] >= g.cfg.ExactFailureBlockAfter {
		return ToolGuardBlock, fmt.Sprintf(
			"同一调用（%s）已连续失败 %d 次，已阻断", name, g.identicalFails[fp])
	}

	return ToolGuardAllow, ""
}

// AfterCall 执行后判定并更新计数。
func (g *ToolCallGuardrail) AfterCall(name, args, output string, isError bool) (ToolGuardAction, string) {
	if g == nil {
		return ToolGuardAllow, ""
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	fp := toolCallFingerprint(name, args)
	g.identicalCalls[fp]++
	if isError {
		g.identicalFails[fp]++
		g.toolFails[name]++
	} else {
		g.identicalFails[fp] = 0
	}

	// 无进展：结果与上一次完全一致（成功但原地打转同样算）
	hash := resultFingerprint(output)
	if hash == g.lastResultHash {
		g.noProgress++
	} else {
		g.lastResultHash = hash
		g.noProgress = 1
	}

	if !g.cfg.WarningsEnabled {
		return ToolGuardAllow, ""
	}

	if g.cfg.HardStopEnabled {
		switch {
		case g.cfg.SameToolFailureHaltAfter > 0 && g.toolFails[name] >= g.cfg.SameToolFailureHaltAfter:
			return ToolGuardHalt, fmt.Sprintf("工具 %s 已连续失败 %d 次，结束回合", name, g.toolFails[name])
		case g.cfg.NoProgressBlockAfter > 0 && g.noProgress >= g.cfg.NoProgressBlockAfter:
			return ToolGuardHalt, fmt.Sprintf("连续 %d 次工具结果完全相同（无进展），结束回合", g.noProgress)
		}
	}

	switch {
	case g.cfg.SameToolFailureWarnAfter > 0 && g.toolFails[name] >= g.cfg.SameToolFailureWarnAfter:
		return ToolGuardWarn, fmt.Sprintf(
			"工具 %s 已失败 %d 次，请换一种做法或先向用户说明原因", name, g.toolFails[name])
	case g.cfg.ExactFailureWarnAfter > 0 && g.identicalFails[fp] >= g.cfg.ExactFailureWarnAfter:
		return ToolGuardWarn, fmt.Sprintf(
			"同一调用已连续失败 %d 次，不要再原样重试", g.identicalFails[fp])
	case g.cfg.NoProgressWarnAfter > 0 && g.noProgress >= g.cfg.NoProgressWarnAfter:
		return ToolGuardWarn, fmt.Sprintf("连续 %d 次工具结果相同，可能没有进展", g.noProgress)
	}

	return ToolGuardAllow, ""
}

// toolCallFingerprint 调用指纹：工具名 + 参数。
func toolCallFingerprint(name, args string) string {
	return name + "\x00" + args
}

// resultFingerprint 结果指纹：只取摘要，避免为比对保留整份输出。
func resultFingerprint(output string) string {
	sum := sha256.Sum256([]byte(output))

	return hex.EncodeToString(sum[:8])
}
