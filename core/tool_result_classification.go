package core

import (
	"encoding/json"
	"strings"
)

// 工具结果的副作用判定。
//
// 对齐 hermes-agent 的 tool_result_classification：回滚 / 重放一段历史之前，
// 必须知道"这段历史里的工具是不是已经改动了外部状态"—— 只读工具（读文件、
// 列目录）重跑是安全的，写入类工具重跑会产生重复副作用。
//
// 判定分两步（与 Python 一致）：
//   - ToolMayHaveSideEffect：该工具原则上会不会改状态；
//   - ClassifyEffectDisposition：这一次调用是否真的落地了 —— 写入类工具
//     在报错之前可能已经写了一部分。

// fileMutatingToolNames 会改动外部状态的工具。
var fileMutatingToolNames = map[string]bool{
	"write_file":       true,
	"delete_path":      true,
	"create_directory": true,
	"terminal":         true, // 任意命令都可能改状态
	"memory":           true, // 写记忆文件
}

// noEffectToolNames 明确只读的工具：重跑安全。
var noEffectToolNames = map[string]bool{
	"read_file":        true,
	"list_directory":   true,
	"background_tasks": true, // 只查询后台任务状态
	"vision_analyze":   true, // 只读图片，不改任何状态
	"glob":             true, // 文件发现，底层 ripgrep 只读
	"grep":             true, // 内容搜索，底层 ripgrep 只读
}

// EffectDisposition 一次工具调用的副作用落地情况。
type EffectDisposition string

const (
	// EffectNone 该工具不产生副作用（只读）。
	EffectNone EffectDisposition = "none"
	// EffectLanded 副作用已确认落地。
	EffectLanded EffectDisposition = "landed"
	// EffectUnknown 可能已产生副作用但无法确认（命令类 / 未知工具 / 报错早退）。
	EffectUnknown EffectDisposition = "unknown"
)

// ToolMayHaveSideEffect 判断工具原则上是否会改变外部状态。
//
// 未知工具（MCP 远端工具等）按"可能有副作用"处理 —— 保守判定只会少一次
// 回滚机会，不会造成错误重放。
func ToolMayHaveSideEffect(name string) bool {
	return !noEffectToolNames[name]
}

// ClassifyEffectDisposition 判定本次调用的副作用落地情况。
func ClassifyEffectDisposition(name, output string, isError bool) EffectDisposition {
	if !ToolMayHaveSideEffect(name) {
		return EffectNone
	}

	if !isError {
		// 命令类工具即便成功也无法确认具体改了什么，仍标 unknown
		if name == "terminal" {
			return EffectUnknown
		}

		return EffectLanded
	}

	// 失败：写入类工具可能在报错前已经写了一部分，看结果里的落地标记
	if fileMutationResultLanded(name, output) {
		return EffectLanded
	}

	return EffectUnknown
}

// fileMutationResultLanded 从写入类工具的结果里判断"写是否真的发生了"。
//
// 工具错误统一以 {"error": "..."} 形式返回（本仓库约定），因此这里只认明确
// 的正向标记（字段名取自 tools 包的实际返回结构）：
//
//	write_file       → bytes_written > 0
//	create_directory → created == true
//	memory           → success == true
func fileMutationResultLanded(name, output string) bool {
	var payload struct {
		BytesWritten *int64 `json:"bytes_written"`
		Created      *bool  `json:"created"`
		Success      *bool  `json:"success"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &payload); err != nil {
		return false
	}

	switch name {
	case "write_file":
		return payload.BytesWritten != nil && *payload.BytesWritten > 0
	case "create_directory":
		return payload.Created != nil && *payload.Created
	default:
		return payload.Success != nil && *payload.Success
	}
}
