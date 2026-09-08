package service

import "switchdev/proxy"

// ModelService 模型管理服务（暴露给前端）
type ModelService struct {
	core *Core
}

func NewModelService(core *Core) *ModelService {
	return &ModelService{core: core}
}

// ModelDetail 模型详情（前端展示用）
type ModelDetail struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Upstream  string `json:"upstream"`
	Stream    bool   `json:"stream"`
	Context   int    `json:"context"`
	Output    int    `json:"output"`
	Vision    bool   `json:"vision"`
	ToolCall  bool   `json:"toolCall"`
	Reasoning bool   `json:"reasoning,omitempty"`
	Free      bool   `json:"free,omitempty"`   // 限时免费标识
	Source    string `json:"source,omitempty"` // live=接口实时/DB 回读 | local=种子白名单 | free=供应商
}

// GetModels 获取全部可用模型（读动态目录快照）
func (s *ModelService) GetModels() []*ModelDetail {
	var result []*ModelDetail

	// auto 虚拟模型（实际走向由运行模式的降级链决定）
	result = append(result, &ModelDetail{
		ID: "auto", Label: "Auto（按运行模式降级链）",
		Upstream: "auto", Stream: true, ToolCall: true,
	})

	for _, m := range proxy.CatalogSnapshot() {
		source := "local"
		if m.Live {
			source = "live"
		}
		result = append(result, &ModelDetail{
			ID: m.ID, Label: m.Label, Upstream: m.Upstream,
			Stream: m.Stream, Context: m.Context, Output: m.Output,
			Vision: m.Vision, ToolCall: m.ToolCall, Reasoning: m.Reasoning,
			Free: m.Free, Source: source,
		})
	}

	// 供应商模型（动态注册的 verified 模型）
	for _, m := range proxy.ProviderModelList() {
		result = append(result, &ModelDetail{
			ID: m.InternalID, Label: m.Label, Upstream: m.ProviderID,
			Stream: true, Context: m.Context, Free: true, Source: "free",
		})
	}

	return result
}
