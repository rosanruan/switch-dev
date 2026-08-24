package providerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// HealthReporter 健康状态变化回调（由 service 层实现，推送前端事件）
type HealthReporter interface {
	EmitProviderHealth(health map[string]map[string]bool)
}

// Monitor 免费模型健康监控（每 5 分钟探测所有 verified 模型）
// 只监控 providerapi 的模型，不影响内置 4 上游
type Monitor struct {
	mgr       *Manager
	client    *http.Client
	reporter  HealthReporter
	threshold int // 连续失败阈值
	onHealth  func(providerID, modelID string, healthy bool)
	closeCh   chan struct{}
	closeOnce sync.Once
}

// NewMonitor 创建监控器
func NewMonitor(mgr *Manager, reporter HealthReporter) *Monitor {
	return &Monitor{
		mgr:       mgr,
		client:    &http.Client{Timeout: 30 * time.Second},
		reporter:  reporter,
		threshold: 2, // 连续失败 2 次标记不健康
		closeCh:   make(chan struct{}),
	}
}

// SetOnHealth 设置单模型健康变化回调（供降级链健康查询使用）
func (m *Monitor) SetOnHealth(fn func(providerID, modelID string, healthy bool)) {
	m.onHealth = fn
}

// Start 启动后台监控（每 5 分钟一轮）
func (m *Monitor) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		// 启动后先跑一轮，之后每 5 分钟
		m.runOnce(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.runOnce(ctx)
			}
		}
	}()
}

// Stop 停止监控
func (m *Monitor) Stop() {
	m.closeOnce.Do(func() { close(m.closeCh) })
}

// runOnce 探测一轮所有 verified 模型
func (m *Monitor) runOnce(ctx context.Context) {
	verified := m.mgr.GetVerifiedModels()
	if len(verified) == 0 {
		return
	}
	changed := false
	var mu sync.Mutex
	var wg sync.WaitGroup
	healthState := map[string]map[string]bool{}

	for pid, models := range verified {
		p := m.mgr.GetProvider(pid)
		if p == nil {
			continue
		}
		for _, mo := range models {
			modelID := mo.ID
			wg.Add(1)
			go func(providerID string, p *ProviderConfig, mid string) {
				defer wg.Done()
				ok := m.probe(ctx, p, mid)
				mu.Lock()
				if healthState[providerID] == nil {
					healthState[providerID] = map[string]bool{}
				}
				healthState[providerID][mid] = ok
				mu.Unlock()

				if ok {
					wasHealthy := m.mgr.GetModel(providerID, mid)
					if wasHealthy != nil && !wasHealthy.Healthy {
						m.mgr.ResetFailCount(providerID, mid)
						changed = true
						if m.onHealth != nil {
							m.onHealth(providerID, mid, true)
						}
					}
				} else {
					justUnhealthy := m.mgr.BumpFailCount(providerID, mid, m.threshold)
					if justUnhealthy {
						changed = true
						if m.onHealth != nil {
							m.onHealth(providerID, mid, false)
						}
					}
				}
			}(pid, p, modelID)
		}
	}
	wg.Wait()

	if changed && m.reporter != nil {
		m.reporter.EmitProviderHealth(healthState)
	}
}

// probe 对单个模型发轻量探测请求
// 用 max_tokens=1 的最小请求验证模型可用；按供应商协议选择端点与鉴权头
// （anthropic 走 /v1/messages + x-api-key，openai 走 /chat/completions + Bearer）。
func (m *Monitor) probe(ctx context.Context, p *ProviderConfig, modelID string) bool {
	payload := map[string]any{
		"model":      modelID,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}

	proto := p.EffectiveProtocol()
	var url string
	if proto == ProtocolAnthropic {
		url = trimSlash(p.BaseURL) + "/v1/messages"
	} else {
		url = trimSlash(p.BaseURL) + "/chat/completions"
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if proto == ProtocolAnthropic {
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == 200
}

// GetHealth 当前所有 verified 模型的健康状态 map[providerID][modelID]bool
func (m *Monitor) GetHealth() map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for pid, models := range m.mgr.GetVerifiedModels() {
		out[pid] = map[string]bool{}
		for _, mo := range models {
			out[pid][mo.ID] = mo.Healthy
		}
	}
	return out
}

// IsHealthy 查询某模型是否健康（供降级链权重判断；不存在视为健康）
func (m *Monitor) IsHealthy(providerID, modelID string) bool {
	mo := m.mgr.GetModel(providerID, modelID)
	if mo == nil {
		return true
	}
	return mo.Healthy
}

// trimSlash 去掉末尾斜杠
func trimSlash(s string) string {
	if len(s) > 0 && s[len(s)-1] == '/' {
		return s[:len(s)-1]
	}
	return s
}
