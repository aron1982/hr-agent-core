package core

import (
	"fmt"
	"strconv"
	"strings"
)

// 段摘要 prompt：五区 schema（设计文档 §4.5）+ 跨段规则（§4.6）。
//
// 结构固定是整套机制的地基：快照折叠靠机械解析"状态变化"行、远段衰减靠
// 机械解析"关键点"行、缺区校验决定是否降级——段落格式漂移会同时打断这三者。
const compactPromptVersion = 1

// 段摘要输出的分隔标记。模型按契约输出两块：回改后的上一段摘要（可选）
// 与本段新摘要。解析失败即视为本次总结失败，走降级路径。
const (
	reviseMarker  = "<<<REVISED_PREV>>>"
	summaryMarker = "<<<NEW_SUMMARY>>>"
	unchangedMark = "UNCHANGED"
)

// 五区 schema 的区标题（模型必须原样输出这些标题行）。
const (
	secKeyPoints  = "◆ 关键点"
	secDecisions  = "◆ 决策"
	secCausality  = "◆ 因果链"
	secTodos      = "◆ 待办"
	secUserWords  = "◆ 用户原话要点"
	secStateDelta = "◆ 状态变化"
)

// buildSegmentPrompt 构造单段总结的 prompt。
//
//	segNo      段号（1 起）
//	prevSummary 上一段摘要全文（首段为空）
//	overlap     上一段尾部重叠区原文（可为空）
//	segment     本段原文
func buildSegmentPrompt(segNo int, prevSummary, overlap, segment string) string {
	var b strings.Builder
	b.WriteString("你是会话历史的分段摘要器。下面给出某会话一段的原始消息（可能附带上一段尾部的重叠区与上一段摘要）。")
	b.WriteString("请严格按固定标记输出两部分，不要输出任何其他内容。\n\n")

	b.WriteString("输出格式：\n")
	b.WriteString(reviseMarker + "\n")
	b.WriteString("（若提供了上一段摘要：输出回改后的上一段摘要全文。只允许两类改动：")
	b.WriteString("① 重叠区内容已由本段摘要覆盖的条目，从上一段删除；")
	b.WriteString("② 为上一段悬而未决的条目补一句「后续见本段摘要」的指向，不新增事实。")
	b.WriteString("无需改动时输出 " + unchangedMark + "）\n")
	b.WriteString("（若未提供上一段摘要：直接输出 " + unchangedMark + "）\n")
	b.WriteString(summaryMarker + "\n")
	b.WriteString("[S" + strconv.Itoa(segNo) + "]\n")
	b.WriteString(secKeyPoints + "：<事实/数字/日期，每条一行，行末附 [锚点: 原文原词]>\n")
	b.WriteString(secDecisions + "：<定了什么、谁定的、为什么>\n")
	b.WriteString(secCausality + "：<前因后果按时间序，一行一步>\n")
	b.WriteString(secTodos + "：<明确的待办与悬而未决的问题>\n")
	b.WriteString(secUserWords + "：<用户明确说过的重要要求，尽量原话>\n")
	b.WriteString(secStateDelta + "：<主体·属性 旧→新，每条一行；本段无状态变化则整个省略本区>\n\n")

	b.WriteString("要求：\n")
	b.WriteString("- 锚点词必须是原文中的原词（人名/编号/路径/特征短语），供全文检索定位\n")
	b.WriteString("- 不确定是否重要的信息宁可保留，宁多勿漏\n")
	b.WriteString("- 不确定是否重要的信息宁可保留；首次出现的任务背景（无上一段摘要时）额外增加一区 ◆ 任务目标：用户最初要做什么（引用原话关键词）\n")

	if prevSummary != "" {
		b.WriteString("\n上一段摘要（供回改参考，原样输出到 " + reviseMarker + " 块）：\n" + prevSummary + "\n")
	}
	if overlap != "" {
		b.WriteString("\n" + overlap + "\n")
	}
	b.WriteString("\n本段原始消息：\n" + segment)

	return b.String()
}

// parseSegmentOutput 解析段摘要输出。
//
// 回改块解析失败或内容不合法时返回空 revisedPrev——调用方保留旧摘要原样
// （宁重复不失真）。新摘要缺必需结构时 ok=false，调用方整体降级。
func parseSegmentOutput(out string) (revisedPrev, summary string, ok bool) {
	idx := strings.Index(out, summaryMarker)
	if idx < 0 {
		return "", "", false
	}
	summary = strings.TrimSpace(out[idx+len(summaryMarker):])
	if !validateSegmentSummary(summary) {
		return "", "", false
	}

	if rv := strings.Index(out, reviseMarker); rv >= 0 && rv < idx {
		revised := strings.TrimSpace(out[rv+len(reviseMarker) : idx])
		switch {
		case revised == "" || revised == unchangedMark:
			revisedPrev = ""
		case validateSegmentSummary(revised):
			revisedPrev = revised
		default:
			// 回改块缺结构：保留旧摘要（宁重复不失真）
			revisedPrev = ""
		}
	}

	return revisedPrev, summary, true
}

// validateSegmentSummary 段摘要必须含"关键点"区——它是远段衰减与检索锚点的
// 唯一载体，其余区缺失可以容忍（模型可能合理省略"待办"等空区）。
func validateSegmentSummary(s string) bool {
	return strings.Contains(s, secKeyPoints)
}

// keyPointLines 提取段摘要的关键点行（远段衰减用）。
// 模型被要求每条一行、行首带区标题；带不出就返回空，由调用方整段保留。
func keyPointLines(summary string) []string {
	var lines []string
	for _, line := range strings.Split(summary, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), secKeyPoints) {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

// foldSnapshot 从各段摘要的"状态变化"行按段序折叠出当前状态快照。
//
// 纯机械解析，零 LLM 成本。行格式：`主体·属性 旧→新`（`→` 后取当前值）；
// 含"不变/无变化"的行跳过；解析不了的行静默跳过——快照是可重算的派生物，
// 丢一行不如渲染错一行。
func foldSnapshot(prev string, segs []SegmentSummary) string {
	type entry struct{ key, val string }
	var order []string
	vals := map[string]string{}

	// 种子：上一版快照行「· key：value」
	put := func(key, val string) {
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if key == "" || val == "" {
			return
		}
		if _, ok := vals[key]; !ok {
			order = append(order, key)
		}
		vals[key] = val
	}
	for _, line := range strings.Split(prev, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "· ") {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(line, "· "), "：", 2)
		if len(parts) != 2 {
			continue
		}
		put(parts[0], parts[1])
	}

	// 按段序应用状态变化（回改过的段以最新版本参与折叠）
	for _, seg := range segs {
		for _, line := range strings.Split(seg.Summary, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, secStateDelta+"：") {
				continue
			}
			rest := strings.TrimSpace(strings.TrimPrefix(line, secStateDelta+"："))
			sp := strings.SplitN(rest, " ", 2)
			if len(sp) != 2 {
				continue
			}
			trans := strings.TrimSpace(sp[1])
			if strings.Contains(trans, "不变") || strings.Contains(trans, "无变化") {
				continue
			}
			if idx := strings.LastIndex(trans, "→"); idx >= 0 {
				put(sp[0], trans[idx+len("→"):])
			}
		}
	}

	if len(order) == 0 {
		return ""
	}
	var b strings.Builder
	for _, k := range order {
		fmt.Fprintf(&b, "· %s：%s\n", k, vals[k])
	}
	return strings.TrimSpace(b.String())
}
