package proxy

import (
	"strings"

	"switchdev/upstream"
)

// MergeModels 合并接口实时模型 + 本地映射元数据 + auto 虚拟模型
// fetched: 从上游接口拉取的模型列表（nil 表示拉取失败，回退本地白名单）
// upstreamName: "joycode" | "deveco" | "opencode"
// 返回合并后的模型列表（供前端设置页显示 + 配置选择）
func MergeModels(upstreamName string, fetched []upstream.FetchedModel, fetchOK bool) []ModelInfo {
	var result []ModelInfo

	// OpenCode 只展示 free 模型（非 free 的需付费/会员，代理不接入）
	if upstreamName == "opencode" && fetchOK {
		// 新建 slice，不能用 fetched[:0] —— 那会原地改写调用方入参的底层数组
		filtered := make([]upstream.FetchedModel, 0, len(fetched))
		for _, fm := range fetched {
			if isFreeModel(fm.ID) {
				filtered = append(filtered, fm)
			}
		}
		fetched = filtered
	}

	if fetchOK && len(fetched) > 0 {
		// 接口拉取成功：以接口模型为主，本地映射补充元数据
		for _, fm := range fetched {
			mi := ModelInfo{
				ID:        fm.ID,
				Label:     fm.Label,
				Upstream:  upstreamName,
				Stream:    fm.Stream,
				Context:   fm.Context,
				Output:    fm.Output,
				Vision:    fm.Vision,
				ToolCall:  fm.ToolCall,
				Reasoning: fm.Reasoning,
				Object:    "model",
				Created:   1700000000,
				// 接口返回的 ID 就是发往上游的真实 model 名。必须在 applyLocalMeta
				// 之前记下来 —— DevEco 分支会把 mi.ID 覆写成本地内部 id（GLM-5.1 -> glm-5.1）。
				Wire: fm.ID,
			}

			// 用本地映射表补充/修正元数据
			applyLocalMeta(&mi, upstreamName)

			if mi.Label == "" {
				mi.Label = fm.ID
			}
			result = append(result, mi)
		}
	} else {
		// 接口拉取失败：回退本地静态白名单
		result = append(result, localModels(upstreamName)...)
	}

	// 追加本地 auto 虚拟模型（DevEco/JoyCode 保留 auto，OpenCode 不加）
	if upstreamName != "opencode" {
		if !hasModelID(result, "auto") {
			result = append([]ModelInfo{{
				ID: "auto", Object: "model", Created: 1700000000,
				OwnedBy:  "multi",
				Label:    "Auto（" + autoLabel(upstreamName) + "）",
				Stream:   true, Upstream: upstreamName, ToolCall: true,
			}}, result...)
		}
	}

	return result
}

// applyLocalMeta 用本地映射表补充元数据（接口没返回的字段从本地白名单取）
func applyLocalMeta(mi *ModelInfo, upstreamName string) {
	switch upstreamName {
	case "joycode":
		// 接口返回的 ID 就是 chatApiModel，与本地 JoyCodeModelByID 一致
		if lm := JoyCodeModelByID[mi.ID]; lm != nil {
			if mi.Output == 0 {
				mi.Output = lm.OutputMaxTokens
			}
			if mi.Label == "" || mi.Label == mi.ID {
				mi.Label = lm.Label
			}
			mi.Stream = lm.Stream // 本地的 stream 标记更准（接口的 supportStream 对部分模型不准）
			mi.Free = lm.Free
		}
	case "deveco":
		// 接口返回 model_id（如 GLM-5.1），需反向映射到本地 id（如 glm-5.1）
		// 通过 DevEcoLabelToID 反查：upstream 名 -> 本地 id
		lowID := strings.ToLower(mi.ID)
		if localID, ok := DevEcoLabelToID[lowID]; ok {
			// 把接口的 model_id 替换成本地可识别的 id
			mi.ID = localID
			if lm := DevEcoModelByID[localID]; lm != nil {
				if mi.Label == "" || mi.Label == mi.ID {
					mi.Label = lm.Label
				}
				mi.Free = lm.Free
			}
		}
		// DevEco 本地白名单的 context/output 通常更准（接口实测一致）
	case "opencode":
		// OpenCode 接口只返回 id，用本地白名单补 context/output（仅 free 模型有）
		if lm := OpenCodeModelByID[mi.ID]; lm != nil {
			mi.Context = lm.Context
			mi.Output = lm.Output
			mi.Stream = true
			mi.ToolCall = true
			mi.Free = lm.Free
		}
	case "workbuddy":
		// 动态拉取的模型加 wb/ 前缀（种子模型已有），
		// 确保路由层能识别为 WorkBuddy 模型。
		if !strings.HasPrefix(mi.ID, "wb/") {
			mi.ID = "wb/" + mi.ID
		}
		// Wire 已在 MergeModels 中设为 fm.ID（无前缀），无需再改。
		if lm := WorkBuddyModelByID[mi.ID]; lm != nil {
			if mi.Label == "" || mi.Label == mi.ID {
				mi.Label = lm.Label
			}
			if mi.Output == 0 {
				mi.Output = lm.Output
			}
			if mi.Context == 0 {
				mi.Context = lm.Context
			}
			mi.Vision = lm.Vision
			mi.ToolCall = lm.ToolCall
			mi.Reasoning = lm.Reasoning
			mi.Free = lm.Free
		}
	}
}

// localModels 本地静态白名单（接口失败时回退）
func localModels(upstreamName string) []ModelInfo {
	switch upstreamName {
	case "joycode":
		var r []ModelInfo
		for _, m := range JoyCodeModels {
			r = append(r, ModelInfo{
				ID: m.ID, Label: m.Label, Upstream: "joycode", Wire: m.ID,
				Stream: m.Stream, Output: m.OutputMaxTokens, ToolCall: true,
				Free: m.Free, Object: "model", Created: 1700000000,
			})
		}
		return r
	case "deveco":
		var r []ModelInfo
		for _, m := range DevEcoModels {
			r = append(r, ModelInfo{
				ID: m.ID, Label: m.Label, Upstream: "deveco", Wire: m.Upstream,
				Stream: true, Context: m.Context, Output: m.Output, ToolCall: true,
				Free: m.Free, Object: "model", Created: 1700000000,
			})
		}
		return r
	case "opencode":
		var r []ModelInfo
		for _, m := range OpenCodeModels {
			r = append(r, ModelInfo{
				ID: m.ID, Label: m.Label, Upstream: "opencode", Wire: m.ID,
				Stream: true, Context: m.Context, Output: m.Output, ToolCall: true,
				Free: m.Free, Object: "model", Created: 1700000000,
			})
		}
		return r
	case "workbuddy":
		var r []ModelInfo
		for _, m := range WorkBuddyModels {
			r = append(r, ModelInfo{
				ID: m.ID, Label: m.Label, Upstream: "workbuddy", Wire: stripWbPrefix(m.ID),
				Stream: true, Context: m.Context, Output: m.Output,
				Vision: m.Vision, ToolCall: m.ToolCall, Reasoning: m.Reasoning, Free: m.Free,
				Object: "model", Created: 1700000000,
			})
		}
		return r
	}
	return nil
}

// hasModelID 检查列表里是否已有某 id
func hasModelID(list []ModelInfo, id string) bool {
	for _, m := range list {
		if m.ID == id {
			return true
		}
	}
	return false
}

// isFreeModel 判断 OpenCode 模型是否为 free（id 以 -free 结尾）
func isFreeModel(id string) bool {
	return strings.HasSuffix(id, "-free")
}

// autoLabel auto 虚拟模型的描述
func autoLabel(upstreamName string) string {
	switch upstreamName {
	case "deveco":
		return "DevEco GLM-5.1"
	case "joycode":
		return "JoyCode 兜底"
	case "workbuddy":
		return "WorkBuddy"
	}
	return ""
}
