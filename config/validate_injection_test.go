package config

import (
	"testing"
)

func TestIsValidUpstreamWithInjectedValidator(t *testing.T) {
	// 保存原始 validator，测试结束恢复
	orig := additionalUpstreamValidator
	defer func() { additionalUpstreamValidator = orig }()

	// 注入一个只认 "groq" 的 validator
	additionalUpstreamValidator = func(id string) bool {
		return id == "groq"
	}

	tests := []struct {
		upstream string
		want     bool
		reason   string
	}{
		{"joycode", true, "内置上游始终合法"},
		{"deveco", true, "内置上游始终合法"},
		{"opencode", true, "内置上游始终合法"},
		{"workbuddy", true, "内置上游始终合法"},
		{"groq", true, "已注册的 ProviderAPI 供应商"},
		{"grop", false, "笔误的 id 应被拦截"},
		{"custom-nonexist", false, "未注册的自定义 id 应被拦截"},
		{"", false, "空字符串始终非法"},
	}

	for _, tt := range tests {
		got := isValidUpstream(tt.upstream)
		if got != tt.want {
			t.Errorf("isValidUpstream(%q) = %v, want %v (%s)", tt.upstream, got, tt.want, tt.reason)
		}
	}
}

func TestIsValidUpstreamWithoutValidator(t *testing.T) {
	// 保存原始 validator，测试结束恢复
	orig := additionalUpstreamValidator
	defer func() { additionalUpstreamValidator = orig }()

	// nil validator → 宽松模式（非空即合法）
	additionalUpstreamValidator = nil

	if !isValidUpstream("any-nonempty-string") {
		t.Error("nil validator 时应回落宽松策略：非空字符串应合法")
	}
	if !isValidUpstream("grop") {
		t.Error("nil validator 时笔误 id 也应放行（运行时 safe-skip 兜底）")
	}
	if isValidUpstream("") {
		t.Error("空字符串应始终非法")
	}
	if !isValidUpstream("joycode") {
		t.Error("内置上游始终合法")
	}
}
