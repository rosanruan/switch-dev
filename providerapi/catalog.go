package providerapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"switchdev/paths"
)

// CatalogProvider 目录中的供应商（来自 provider_apis_catalog.json）
type CatalogProvider struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Region           string         `json:"region"` // "international" | "domestic"
	BaseURL          string         `json:"base_url"`
	GetAPIKeyURL     string         `json:"get_api_key_url"`
	AuthMethod       string         `json:"auth_method"`
	CreditCardReq    bool           `json:"credit_card_required"`
	OpenAICompatible bool           `json:"openai_compatible"`
	Protocol         string         `json:"protocol,omitempty"` // 出站协议：openai（默认）| anthropic；空值按 OpenAICompatible 推断
	MaxContext       string         `json:"max_context"`        // "1M"、"131K" 等文本
	FreeModelsCount  int            `json:"free_models_count"`
	FreeModels       []CatalogModel `json:"free_models"`
}

// EffectiveProtocol 返回目录条目的出站协议：显式 protocol 优先，否则按 openai_compatible 推断
func (c *CatalogProvider) EffectiveProtocol() string {
	if c.Protocol == ProtocolAnthropic || c.Protocol == ProtocolOpenAI {
		return c.Protocol
	}
	if c.OpenAICompatible {
		return ProtocolOpenAI
	}
	return ProtocolAnthropic
}

// CatalogModel 目录中的单个免费模型
type CatalogModel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Context   string `json:"context"`    // "1M"、"131K"、"32K" 文本
	RateLimit string `json:"rate_limit"` // 免费方式/限流描述
	// 能力标记（来自上游目录，用于前端展示/过滤）
	Reasoning        bool `json:"reasoning,omitempty"`         // 思维链/推理模型
	ToolCall         bool `json:"tool_call,omitempty"`         // 支持函数/工具调用
	Vision           bool `json:"vision,omitempty"`            // 支持图片/多模态输入
	StructuredOutput bool `json:"structured_output,omitempty"` // 支持结构化输出（JSON schema）
}

// Catalog 目录根结构
type Catalog struct {
	Providers  []CatalogProvider `json:"providers"`
	Deprecated []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Reason string `json:"reason"`
		Status string `json:"status"`
	} `json:"deprecated"`
}

// embedCatalog 内置目录（由 main 注入：main 里 //go:embed 后调 SetEmbedCatalog）
var embedCatalog []byte

// SetEmbedCatalog 注入内置目录（main 调用，避免 embed 直接依赖路径）
func SetEmbedCatalog(data []byte) {
	embedCatalog = data
}

// GitHubCatalogURL 目录的 GitHub raw 地址
const GitHubCatalogURL = "https://raw.githubusercontent.com/rosanruan/switch-dev/main/data/provider_apis_catalog.json"

// CatalogLoader 目录加载器（embed + GitHub + 本地缓存）
type CatalogLoader struct {
	mu     sync.Mutex
	cached *Catalog // 内存缓存
}

// LoadCatalog 返回目录：优先内存缓存 → GitHub 拉取（成功后存缓存）→ embed 内置兜底
func (l *CatalogLoader) LoadCatalog() *Catalog {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cached != nil {
		return l.cached
	}

	// 1. 尝试 GitHub 拉取（最新）
	if cat, err := l.fetchGitHub(); err == nil && cat != nil {
		l.cached = cat
		return cat
	}

	// 2. 尝试本地缓存（上次 GitHub 拉取成功的副本）
	if cat, err := l.loadCache(); err == nil && cat != nil {
		l.cached = cat
		return cat
	}

	// 3. 回退 embed 内置
	if len(embedCatalog) > 0 {
		var cat Catalog
		if err := json.Unmarshal(embedCatalog, &cat); err == nil {
			l.cached = &cat
			return &cat
		}
	}
	return &Catalog{}
}

// RefreshCatalog 强制从 GitHub 拉最新目录；成功则比较并写本地缓存
// 返回是否更新了目录（内容变化）
func (l *CatalogLoader) RefreshCatalog() (*Catalog, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cat, err := l.fetchGitHub()
	if err != nil {
		// 失败：回退现有缓存/embed
		if l.cached != nil {
			return l.cached, false, nil
		}
		return nil, false, err
	}
	if cat == nil {
		return l.cached, false, nil
	}

	// 比较是否与本地缓存一致
	changed := true
	if cur, err := l.loadCacheUnlocked(); err == nil && cur != nil {
		if catalogEqual(cur, cat) {
			changed = false
		}
	}

	l.cached = cat
	if changed {
		_ = l.writeCacheUnlocked(cat)
	}
	return cat, changed, nil
}

// fetchGitHub 从 GitHub raw 拉取目录
func (l *CatalogLoader) fetchGitHub() (*Catalog, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", GitHubCatalogURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "switch-dev")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub 目录返回 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var cat Catalog
	if err := json.Unmarshal(body, &cat); err != nil {
		return nil, err
	}
	if len(cat.Providers) == 0 {
		return nil, fmt.Errorf("GitHub 目录为空")
	}
	return &cat, nil
}

// loadCache 读本地缓存（带锁外调用约束：内部已加锁时用 loadCacheUnlocked）
func (l *CatalogLoader) loadCache() (*Catalog, error) {
	return l.loadCacheUnlocked()
}

func (l *CatalogLoader) loadCacheUnlocked() (*Catalog, error) {
	data, err := os.ReadFile(paths.ProviderCatalogCachePath())
	if err != nil {
		return nil, err
	}
	var cat Catalog
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, err
	}
	return &cat, nil
}

func (l *CatalogLoader) writeCacheUnlocked(cat *Catalog) error {
	data, err := json.MarshalIndent(cat, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(paths.ProviderCatalogCachePath(), data, 0600)
}

// catalogEqual 比较两个目录是否内容一致（按规范化的 JSON 摘要）
func catalogEqual(a, b *Catalog) bool {
	return hashCatalog(a) == hashCatalog(b)
}

func hashCatalog(c *Catalog) string {
	// 只比较 providers 的 id + name + base_url + models，忽略不稳定字段
	type slim struct {
		ID       string
		BaseURL  string
		ModelIDs []string
	}
	s := struct {
		Providers []slim
	}{}
	for _, p := range c.Providers {
		sp := slim{ID: p.ID, BaseURL: p.BaseURL}
		for _, m := range p.FreeModels {
			sp.ModelIDs = append(sp.ModelIDs, m.ID)
		}
		s.Providers = append(s.Providers, sp)
	}
	data, _ := json.Marshal(s)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
