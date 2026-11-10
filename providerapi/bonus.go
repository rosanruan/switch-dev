package providerapi

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// embedBonusProviders 内置注册送 token 列表（由 main 注入：main 里 //go:embed 后调 SetEmbedBonusProviders）
var embedBonusProviders []byte

// SetEmbedBonusProviders 注入内置注册送 token 列表（main 调用，避免 embed 直接依赖路径）
func SetEmbedBonusProviders(data []byte) {
	embedBonusProviders = data
}

// BonusProvider 注册送 token 的供应商信息
type BonusProvider struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BaseURL     string `json:"baseUrl"`
	RegisterURL string `json:"registerUrl"`
	BonusRule   string `json:"bonusRule"`
}

type bonusProvidersFile struct {
	Providers []BonusProvider `json:"providers"`
}

const GitHubBonusProvidersURL = "https://raw.githubusercontent.com/rosanruan/switch-dev/main/data/bonus_providers.json"

// FetchBonusProviders 从 GitHub 拉取最新注册送 token 供应商列表；
// 远程失败时回退到本地 embed 数据。
func FetchBonusProviders() ([]BonusProvider, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(GitHubBonusProvidersURL)
	if err == nil && resp.StatusCode == 200 {
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		if readErr == nil {
			var f bonusProvidersFile
			if jsonErr := json.Unmarshal(body, &f); jsonErr == nil && len(f.Providers) > 0 {
				return f.Providers, nil
			}
		}
	}
	if resp != nil {
		resp.Body.Close()
	}

	// 回退到本地
	var f bonusProvidersFile
	if err := json.Unmarshal(embedBonusProviders, &f); err != nil {
		return nil, err
	}
	return f.Providers, nil
}
