package proxy

import (
	"strings"
	"sync"
	"sync/atomic"
)

// ====== 模型目录（种子白名单 + 实时拉取 + DB 回读 的统一视图） ======
//
// proxy/models.go 里的四个硬编码 slice 是「种子」：既是首次启动的兜底，
// 也为实时拉取结果补充接口没返回的元数据。目录在种子之上叠加动态条目。
//
// 并发模型：Catalog 构建后不可变，整体经 atomic.Pointer 换指针。
// 请求热路径（ResolveModel/converter 等）只做一次原子 Load，无锁。

// CatalogModel 目录中的单个模型
type CatalogModel struct {
	ID        string // 代理内部 id（含 wb/ 前缀）
	Upstream  string // joycode|deveco|opencode|workbuddy
	WireName  string // 发往上游 base_url 的真实 model 名
	Label     string
	Context   int
	Output    int
	Stream    bool
	Vision    bool
	ToolCall  bool
	Reasoning bool
	Free      bool
	Live      bool // true=实时拉取或 DB 回读；false=仅来自种子
}

// Catalog 不可变模型目录快照
type Catalog struct {
	byID       map[string]*CatalogModel
	aliases    map[string]string // lower(label|wire) -> 内部 id
	byUpstream map[string][]*CatalogModel
	all        []*CatalogModel // 稳定顺序：按 upstreamOrder 分组
}

// upstreamOrder 决定重名时的归属优先级，必须与旧 ResolveModel 的查找顺序一致
// （models.go 里依次查 workbuddy → opencode → deveco → joycode，先命中者胜）
var upstreamOrder = []string{"workbuddy", "opencode", "deveco", "joycode"}

var (
	catalogPtr atomic.Pointer[Catalog]
	seedOnce   sync.Once
)

// currentCatalog 取当前快照；未初始化时懒构建纯种子目录。
// 刻意不用 init()：init 按文件名顺序执行，catalog.go 排在 models.go 之前，
// 那时种子 map 还没建好。
func currentCatalog() *Catalog {
	if c := catalogPtr.Load(); c != nil {
		return c
	}
	seedOnce.Do(func() {
		catalogPtr.CompareAndSwap(nil, BuildCatalog(nil))
	})
	return catalogPtr.Load()
}

// SetCatalog 原子替换目录快照
func SetCatalog(c *Catalog) {
	if c != nil {
		catalogPtr.Store(c)
	}
}

// BuildCatalog 用种子 + 动态条目构建新快照。
// entries 覆盖同 id 种子的元数据（并标记 Live）；entries 里的新 id 直接加入。
func BuildCatalog(entries []CatalogModel) *Catalog {
	seed := SeedCatalogEntries()

	merged := make(map[string]*CatalogModel, len(seed)+len(entries))
	for i := range seed {
		m := seed[i]
		if _, exists := merged[m.ID]; exists {
			continue // 种子内部重名：先到先得（upstreamOrder 保证顺序）
		}
		merged[m.ID] = &m
	}
	for i := range entries {
		e := entries[i]
		if e.ID == "" || e.Upstream == "" {
			continue
		}
		e.Live = true
		if e.WireName == "" {
			e.WireName = defaultWireName(e.ID, e.Upstream)
		}
		if e.Label == "" {
			e.Label = e.ID
		}
		if base, ok := merged[e.ID]; ok && base.Upstream == e.Upstream {
			// 同上游同 id：动态数据为主，种子补空缺
			if e.Context == 0 {
				e.Context = base.Context
			}
			if e.Output == 0 {
				e.Output = base.Output
			}
		} else if ok {
			// 与其它上游的种子模型重名：种子优先，避免动态条目劫持既有路由
			continue
		}
		merged[e.ID] = &e
	}

	c := &Catalog{
		byID:       make(map[string]*CatalogModel, len(merged)),
		aliases:    make(map[string]string, len(merged)*2),
		byUpstream: make(map[string][]*CatalogModel),
	}
	for id, m := range merged {
		c.byID[id] = m
	}

	// 稳定顺序：按上游优先级分组，组内保持种子顺序、动态条目附后
	seen := make(map[string]bool, len(merged))
	appendModel := func(m *CatalogModel) {
		if m == nil || seen[m.ID] {
			return
		}
		seen[m.ID] = true
		c.all = append(c.all, m)
		c.byUpstream[m.Upstream] = append(c.byUpstream[m.Upstream], m)
	}
	for _, up := range upstreamOrder {
		for i := range seed {
			if seed[i].Upstream == up {
				appendModel(c.byID[seed[i].ID])
			}
		}
		for i := range entries {
			if entries[i].Upstream == up {
				appendModel(c.byID[entries[i].ID])
			}
		}
	}
	// 非内置上游的动态条目（理论上不该有，防御性收尾）
	for i := range entries {
		appendModel(c.byID[entries[i].ID])
	}

	// 别名表：label / wire 名反查内部 id。真实 id 永远优先，别名不得遮蔽 id。
	for _, m := range c.all {
		for _, alias := range []string{m.Label, m.WireName} {
			low := strings.ToLower(strings.TrimSpace(alias))
			if low == "" {
				continue
			}
			if _, isRealID := c.byID[low]; isRealID {
				continue
			}
			if _, taken := c.aliases[low]; taken {
				continue // 先到先得，与 upstreamOrder 一致
			}
			c.aliases[low] = m.ID
		}
	}

	return c
}

// SeedCatalogEntries 把四个硬编码 slice 扁平化为目录条目（顺序 = upstreamOrder）
func SeedCatalogEntries() []CatalogModel {
	out := make([]CatalogModel, 0,
		len(WorkBuddyModels)+len(OpenCodeModels)+len(DevEcoModels)+len(JoyCodeModels))

	for _, m := range WorkBuddyModels {
		out = append(out, CatalogModel{
			ID: m.ID, Upstream: "workbuddy", WireName: stripWbPrefix(m.ID),
			Label: m.Label, Context: m.Context, Output: m.Output,
			Stream: true, Vision: m.Vision, ToolCall: m.ToolCall, Reasoning: m.Reasoning, Free: m.Free,
		})
	}
	for _, m := range OpenCodeModels {
		out = append(out, CatalogModel{
			ID: m.ID, Upstream: "opencode", WireName: m.ID,
			Label: m.Label, Context: m.Context, Output: m.Output,
			Stream: true, ToolCall: true, Free: m.Free,
		})
	}
	for _, m := range DevEcoModels {
		out = append(out, CatalogModel{
			ID: m.ID, Upstream: "deveco", WireName: m.Upstream,
			Label: m.Label, Context: m.Context, Output: m.Output,
			Stream: true, ToolCall: true, Free: m.Free,
		})
	}
	for _, m := range JoyCodeModels {
		out = append(out, CatalogModel{
			ID: m.ID, Upstream: "joycode", WireName: m.ID,
			Label: m.Label, Output: m.OutputMaxTokens,
			Stream: m.Stream, ToolCall: true, Free: m.Free,
		})
	}
	return out
}

// defaultWireName 动态条目未显式给 wire 名时的推导
func defaultWireName(id, upstreamName string) string {
	if upstreamName == "workbuddy" {
		return stripWbPrefix(id)
	}
	return id
}

// LookupModel 按内部 id 查目录
func LookupModel(internalID string) (*CatalogModel, bool) {
	m, ok := currentCatalog().byID[internalID]
	return m, ok
}

// lookupByAlias 按 label / wire 名反查内部 id。
// 也接受大小写不同的内部 id（旧实现经 DevEcoLabelToID/JoyCodeLabelToID 做过
// lower() 匹配，DevEco 的 wire 名 "GLM-5.1" 就是靠这条命中 "glm-5.1" 的）。
func lookupByAlias(name string) (string, bool) {
	c := currentCatalog()
	low := strings.ToLower(name)
	if m, ok := c.byID[low]; ok {
		return m.ID, true
	}
	id, ok := c.aliases[low]
	return id, ok
}

// CatalogModelsByUpstream 取某上游的模型（稳定顺序）
func CatalogModelsByUpstream(upstreamName string) []*CatalogModel {
	return currentCatalog().byUpstream[upstreamName]
}

// CatalogSnapshot 取全部模型（稳定顺序）
func CatalogSnapshot() []*CatalogModel {
	return currentCatalog().all
}

// ====== 供应商模型快照 ======
// 与内置目录同样走原子指针：供应商增删会在 Wails RPC goroutine 上重建列表，
// 而 /v1/models 在 HTTP goroutine 上读，直接读写包级 slice 是 data race。

var providerModelsPtr atomic.Pointer[[]ProviderModel]

// ProviderModelList 取当前供应商模型快照（只读，勿修改返回的 slice）
func ProviderModelList() []ProviderModel {
	if p := providerModelsPtr.Load(); p != nil {
		return *p
	}
	return nil
}
