package pricing

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"switchdev/paths"
)

// Price 单个模型的费率（每百万 token 的成本，美元）
type Price struct {
	ModelID          string  `json:"model_id"`
	DisplayName      string  `json:"display_name"`
	InputPerMillion  float64 `json:"input_cost_per_million"`
	OutputPerMillion float64 `json:"output_cost_per_million"`
	CacheRead        float64 `json:"cache_read_cost_per_million"`
	CacheCreation    float64 `json:"cache_creation_cost_per_million"`
}

// pricingFile 自有费率库文件
type pricingFile struct {
	Version int      `json:"version"`
	Prices  []*Price `json:"prices"`
}

// Manager 费率管理器（自有库，持久化 JSON，支持增删改查）
type Manager struct {
	mu     sync.RWMutex
	prices map[string]*Price // key = ModelID
	path   string            // 自有库文件路径
}

// DefaultPath 自有费率库默认路径
func DefaultPath() string {
	return filepath.Join(paths.AppConfigDir(), "pricing.json")
}

// NewManager 创建费率管理器
func NewManager() *Manager {
	return &Manager{
		prices: make(map[string]*Price),
		path:   DefaultPath(),
	}
}

// SetPath 自定义费率库路径（测试用）
func (m *Manager) SetPath(p string) { m.path = p }

// Load 加载费率库：
// - 自有库文件存在：直接加载
// - 不存在：从内置硬编码 DefaultRates 还原并保存，之后用自有库
// 不再依赖 cc-switch.db
func (m *Manager) Load() (imported bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. 自有库存在则直接加载
	if data, err := os.ReadFile(m.path); err == nil {
		var f pricingFile
		if json.Unmarshal(data, &f) == nil && len(f.Prices) > 0 {
			m.prices = make(map[string]*Price, len(f.Prices))
			for _, p := range f.Prices {
				m.prices[p.ModelID] = p
			}
			// 升级合并：内置库后续新增的模型（如 mcloud 渠道、GLM-5.3）补进已有自有库，
			// 否则老用户的 pricing.json 一旦生成，新模型的费率永远进不来（不计费）。
			// 只补缺、不覆盖 —— 用户自定义过费率的条目保持原样。
			added := 0
			for i := range DefaultRates {
				p := DefaultRates[i]
				if _, ok := m.prices[p.ModelID]; !ok {
					m.prices[p.ModelID] = &p
					added++
				}
			}
			if added > 0 {
				if saveErr := m.saveLocked(); saveErr != nil {
					return false, fmt.Errorf("合并内置新增费率后保存自有库失败: %w", saveErr)
				}
			}
			return false, nil
		}
		// 文件存在但损坏/空 -> 用硬编码重建
		m.prices = make(map[string]*Price)
	}

	// 2. 从内置硬编码还原
	if len(DefaultRates) > 0 {
		m.prices = make(map[string]*Price, len(DefaultRates))
		for i := range DefaultRates {
			p := DefaultRates[i]
			m.prices[p.ModelID] = &p
		}
		// 保存到自有库
		if saveErr := m.saveLocked(); saveErr != nil {
			return true, fmt.Errorf("还原硬编码费率但保存自有库失败: %w", saveErr)
		}
		return true, nil
	}
	return false, fmt.Errorf("无内置费率且自有库为空")
}

// saveLocked 保存到自有库（需持锁）
func (m *Manager) saveLocked() error {
	if m.path == "" {
		m.path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0700); err != nil {
		return err
	}
	list := make([]*Price, 0, len(m.prices))
	for _, p := range m.prices {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ModelID < list[j].ModelID })
	f := pricingFile{Version: 1, Prices: list}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, data, 0600)
}

// IsLoaded 费率库是否已加载
func (m *Manager) IsLoaded() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.prices) > 0
}

// Count 费率条数
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.prices)
}

// ListPrices 列出全部费率（按 model_id 排序）
func (m *Manager) ListPrices() []*Price {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Price, 0, len(m.prices))
	for _, p := range m.prices {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ModelID < list[j].ModelID })
	return list
}

// GetPrice 查单条费率
func (m *Manager) GetPrice(modelID string) *Price {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.prices[modelID]; ok {
		cp := *p
		return &cp
	}
	return nil
}

// SetPrice 新增/更新费率并保存
func (m *Manager) SetPrice(p *Price) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prices[p.ModelID] = p
	return m.saveLocked()
}

// DeletePrice 删除费率并保存
func (m *Manager) DeletePrice(modelID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.prices, modelID)
	return m.saveLocked()
}

// normalize 归一化模型 id：小写 + 去常见后缀，匹配费率表标准 id
func normalize(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	id = strings.TrimSuffix(id, "-agent")
	id = strings.TrimSuffix(id, "-free")
	return id
}

// Lookup 查某模型的费率（精确 + 归一化），查不到返回 nil
func (m *Manager) Lookup(modelID string) *Price {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.prices[modelID]; ok {
		cp := *p
		return &cp
	}
	if p, ok := m.prices[normalize(modelID)]; ok {
		cp := *p
		return &cp
	}
	return nil
}

// CalculateCost 计算一次请求的费用（美元）
func (m *Manager) CalculateCost(modelID string, inputTokens, outputTokens int) (float64, *Price) {
	p := m.Lookup(modelID)
	if p == nil {
		return 0, nil
	}
	cost := float64(inputTokens)/1_000_000*p.InputPerMillion +
		float64(outputTokens)/1_000_000*p.OutputPerMillion
	return cost, p
}

// AgentLabel 上游显示名
func AgentLabel(upstream string) string {
	switch upstream {
	case "joycode":
		return "京东 JoyCode"
	case "deveco":
		return "华为 DevEco"
	case "opencode":
		return "OpenCode Zen"
	default:
		return upstream
	}
}

// ReplaceAll 用给定费率全量替换本地费率并保存
func (m *Manager) ReplaceAll(prices []*Price) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prices = make(map[string]*Price, len(prices))
	for _, p := range prices {
		m.prices[p.ModelID] = p
	}
	return m.saveLocked()
}

// ParseRatesGoFile 从 rates_default.go 源码解析 DefaultRates 切片
func ParseRatesGoFile(src []byte) ([]*Price, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.AllErrors)
	if err != nil {
		return nil, fmt.Errorf("解析 Go 源码失败: %w", err)
	}

	var result []*Price
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if name.Name != "DefaultRates" || i >= len(spec.Values) {
				continue
			}
			cl, ok := spec.Values[i].(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, elt := range cl.Elts {
				p := parsePriceLit(elt)
				if p != nil {
					result = append(result, p)
				}
			}
		}
		return true
	})

	if len(result) == 0 {
		return nil, fmt.Errorf("未在源码中找到 DefaultRates")
	}
	return result, nil
}

// parsePriceLit 解析单个 Price 复合字面量
func parsePriceLit(expr ast.Expr) *Price {
	cl, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	p := &Price{}
	for _, e := range cl.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "ModelID":
			if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				p.ModelID, _ = strconv.Unquote(bl.Value)
			}
		case "DisplayName":
			if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				p.DisplayName, _ = strconv.Unquote(bl.Value)
			}
		case "InputPerMillion":
			p.InputPerMillion = parseFloatLit(kv.Value)
		case "OutputPerMillion":
			p.OutputPerMillion = parseFloatLit(kv.Value)
		case "CacheRead":
			p.CacheRead = parseFloatLit(kv.Value)
		case "CacheCreation":
			p.CacheCreation = parseFloatLit(kv.Value)
		}
	}
	return p
}

// parseFloatLit 从 ast 字面量解析 float64
func parseFloatLit(expr ast.Expr) float64 {
	bl, ok := expr.(*ast.BasicLit)
	if !ok {
		return 0
	}
	v, err := strconv.ParseFloat(bl.Value, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseFloat 解析字符串为浮点数，失败返回 0
func parseFloat(s string) float64 {
	var v float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &v); err != nil {
		return 0
	}
	return v
}
