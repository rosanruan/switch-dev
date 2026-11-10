package creds

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadCredsParsesDomain 新版 WorkBuddy AI 客户端的 info 文件带 auth.domain：
// 必须解析并归一化（剥协议前缀与斜杠），作为上游 base URL 的事实来源
func TestLoadCredsParsesDomain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workbuddy-desktop-ai.info")
	info := map[string]interface{}{
		"account": map[string]string{"uid": "u1", "nickname": "n"},
		"auth": map[string]interface{}{
			"accessToken": "tok",
			"domain":      "https://www.workbuddy.ai/",
		},
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	m := NewWorkBuddyCredManager(WorkBuddyConfig{
		BaseURL:  "https://copilot.tencent.com/v2",
		InfoPath: path,
	})

	cred, err := m.LoadCreds()
	if err != nil {
		t.Fatalf("LoadCreds: %v", err)
	}
	if cred.Domain != "www.workbuddy.ai" {
		t.Errorf("Domain = %q, want www.workbuddy.ai（应剥掉协议前缀与首尾斜杠）", cred.Domain)
	}
	if got := cred.BaseURL(m.Config().BaseURL); got != "https://www.workbuddy.ai/v2" {
		t.Errorf("BaseURL = %q, want https://www.workbuddy.ai/v2", got)
	}
	if got := cred.Platform(); got != "workbuddy-ai" {
		t.Errorf("Platform = %q, want workbuddy-ai", got)
	}
}

// TestWorkBuddyCredLegacyFallback 旧版 CodeBuddy 的 info 文件没有 auth.domain：
// base URL 与 platform 都回退旧值，老客户端用户不受端点迁移影响
func TestWorkBuddyCredLegacyFallback(t *testing.T) {
	cred := &WorkBuddyCred{AccessToken: "tok", UID: "u1"}
	if got := cred.BaseURL("https://copilot.tencent.com/v2"); got != "https://copilot.tencent.com/v2" {
		t.Errorf("BaseURL = %q, want 配置回退值", got)
	}
	if got := cred.Platform(); got != "workbuddy" {
		t.Errorf("Platform = %q, want workbuddy", got)
	}
}

// TestWorkBuddyAuthHeadersShape 对齐新版 CLI 的 auth 拦截器：
// 不再构造 X-Userinfo；带 X-Product；domain 非空才带 X-Domain
func TestWorkBuddyAuthHeadersShape(t *testing.T) {
	h := WorkBuddyAuthHeaders(&WorkBuddyCred{AccessToken: "tok", UID: "u1", Domain: "www.workbuddy.ai"})
	if h["X-Product"] != "SaaS" {
		t.Errorf("X-Product = %q, want SaaS", h["X-Product"])
	}
	if h["X-Domain"] != "www.workbuddy.ai" {
		t.Errorf("X-Domain = %q, want www.workbuddy.ai", h["X-Domain"])
	}
	if h["X-User-Id"] != "u1" {
		t.Errorf("X-User-Id = %q, want u1", h["X-User-Id"])
	}
	if _, ok := h["X-Userinfo"]; ok {
		t.Error("不应再构造 X-Userinfo，旧格式已是过期指纹")
	}

	legacy := WorkBuddyAuthHeaders(&WorkBuddyCred{AccessToken: "tok", UID: "u1"})
	if _, ok := legacy["X-Domain"]; ok {
		t.Error("无 domain 的旧版凭据不应带 X-Domain")
	}
	if legacy["X-Product"] != "SaaS" {
		t.Errorf("X-Product = %q, want SaaS（产品拦截器加到所有请求）", legacy["X-Product"])
	}
}
