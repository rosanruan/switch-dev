package providerapi

import (
	"bytes"
	"strings"
	"testing"
)

func TestShareBinaryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewManager(dir + "/cfg.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.UpsertProvider(&ProviderConfig{ID: "groq", Name: "Groq", BaseURL: "https://api.groq.com", APIKey: "gsk_secret", Models: []ProviderModel{{ID: "llama", Verified: true, TPS: 82.1}}}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.UpsertProvider(&ProviderConfig{ID: "custom-x", Name: "X", BaseURL: "https://x", APIKey: "k2", Custom: true}); err != nil {
		t.Fatal(err)
	}

	data, err := mgr.EncryptShare([]string{"groq", "custom-x"}, "correct horse battery staple", ShareOptions{IncludeAPIKey: true, Encrypt: true}, "0.0.6", "2026-08-13T00:00:00Z")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// 二进制魔法值 SD（当前写入）
	if !bytes.HasPrefix(data, []byte{'S', 'D'}) {
		t.Fatalf("missing SD magic: %x", data[:2])
	}
	// version=1, encrypted flag set
	if data[2] != 1 || data[3]&flagEncrypted == 0 {
		t.Fatalf("bad header: version=%d flags=%d", data[2], data[3])
	}
	// 不能是可读 JSON
	if bytes.Contains(data, []byte("gsk_secret")) {
		t.Fatal("ciphertext leaks plaintext API key")
	}
	if bytes.Contains(data, []byte(`"providers"`)) {
		t.Fatal("file looks like plaintext JSON")
	}

	// 错误密码应失败
	if _, err := DecryptShare(data, "wrong"); err == nil {
		t.Fatal("expected failure with wrong password")
	}

	// 正确密码解出全部内容
	got, err := DecryptShare(data, "correct horse battery staple")
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 providers, got %d", len(got))
	}
	var ok bool
	for _, sp := range got {
		if sp.ID == "groq" {
			ok = true
			if sp.APIKey != "gsk_secret" {
				t.Fatalf("api key not preserved: %q", sp.APIKey)
			}
			if len(sp.Models) != 1 || sp.Models[0].TPS < 82 {
				t.Fatalf("model/tps lost: %+v", sp.Models)
			}
		}
	}
	if !ok {
		t.Fatal("groq missing after decrypt")
	}

	// 预览只暴露 version/encrypted，不泄露供应商
	prev, err := InspectShare(data)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !prev.Encrypted || prev.Version != 1 {
		t.Fatalf("preview wrong: %+v", prev)
	}
}

func TestShareTamperDetected(t *testing.T) {
	dir := t.TempDir()
	mgr, _ := NewManager(dir + "/c.json")
	_ = mgr.UpsertProvider(&ProviderConfig{ID: "a", Name: "A", BaseURL: "https://a", APIKey: "k"})
	data, err := mgr.EncryptShare([]string{"a"}, "pw", ShareOptions{IncludeAPIKey: true, Encrypt: true}, "0.0.6", "")
	if err != nil {
		t.Fatal(err)
	}
	// 翻转密文中一个字节
	data[len(data)-1] ^= 0xFF
	if _, err := DecryptShare(data, "pw"); err == nil {
		t.Fatal("expected tamper to be detected")
	}
}

func TestShareRejectsNonSDS(t *testing.T) {
	if _, err := InspectShare([]byte(`{"format":"switchdev-share"}`)); err == nil {
		t.Fatal("JSON file should be rejected as binary .sds")
	}
	if _, err := InspectShare([]byte("too short")); err == nil {
		t.Fatal("truncated file should be rejected")
	}
}

// TestShareRejectsLegacySFMagic 魔数已从 SF 改为 SD，旧的 SF 文件应被拒绝（不做向后兼容）。
func TestShareRejectsLegacySFMagic(t *testing.T) {
	mgr, err := NewManager(t.TempDir() + "/cfg.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.UpsertProvider(&ProviderConfig{ID: "groq", Name: "Groq", BaseURL: "https://api.groq.com", APIKey: "gsk_secret", Models: []ProviderModel{{ID: "llama", Verified: true}}}); err != nil {
		t.Fatal(err)
	}
	data, err := mgr.EncryptShare([]string{"groq"}, "hunter2", ShareOptions{IncludeAPIKey: true, Encrypt: true}, "0.0.6", "2026-08-13T00:00:00Z")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// 把魔数第二字节 D 改成旧版的 F
	legacy := append([]byte(nil), data...)
	legacy[1] = 'F'

	if _, err := InspectShare(legacy); err == nil {
		t.Fatal("legacy SF magic must be rejected, but InspectShare accepted it")
	}
	if _, err := DecryptShare(legacy, "hunter2"); err == nil {
		t.Fatal("legacy SF magic must be rejected, but DecryptShare accepted it")
	}
}

func TestGenerateSharePasswordEntropy(t *testing.T) {
	p, err := GenerateSharePassword()
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 6 {
		t.Fatalf("want 6 chars, got %d", len(p))
	}
	if strings.ContainsAny(p, "0Ol1I") {
		t.Fatalf("password contains ambiguous char: %q", p)
	}
}

func TestShareWithoutKeyStillEncrypted(t *testing.T) {
	dir := t.TempDir()
	mgr, _ := NewManager(dir + "/c.json")
	_ = mgr.UpsertProvider(&ProviderConfig{ID: "groq", Name: "Groq", BaseURL: "https://api.groq.com", APIKey: "gsk_secret", Models: []ProviderModel{{ID: "llama", Verified: true, TPS: 82.1}}})

	// 不含 key，但文件仍加密；apiKey 被剥离
	data, err := mgr.EncryptShare([]string{"groq"}, "pw", ShareOptions{IncludeAPIKey: false, Encrypt: true}, "0.0.6", "")
	if err != nil {
		t.Fatal(err)
	}
	if data[3]&flagEncrypted == 0 {
		t.Fatal("expected encrypted flag set")
	}
	if bytes.Contains(data, []byte("gsk_secret")) {
		t.Fatal("API key leaked into ciphertext")
	}
	if bytes.Contains(data, []byte("api.groq.com")) {
		t.Fatal("provider baseURL should be encrypted (not visible in plaintext)")
	}
	// 错密码失败
	if _, err := DecryptShare(data, "wrong"); err == nil {
		t.Fatal("expected wrong password to fail")
	}
	// 正确密码解出，apiKey 为空
	got, err := DecryptShare(data, "pw")
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if len(got) != 1 || got[0].APIKey != "" {
		t.Fatalf("expected 1 provider with empty key, got %+v", got)
	}

	// 不含 key + 空密码：允许（内置密钥混淆模式），文件仍是密文且无需密码即可导入
	obf, err := mgr.EncryptShare([]string{"groq"}, "", ShareOptions{IncludeAPIKey: false}, "0.0.6", "")
	if err != nil {
		t.Fatalf("obfuscated export: %v", err)
	}
	if obf[3]&flagEncrypted == 0 || obf[4] != kdfEmbedded {
		t.Fatalf("expected encrypted+embedded kdf, got flags=%x kdf=%x", obf[3], obf[4])
	}
	if bytes.Contains(obf, []byte("api.groq.com")) {
		t.Fatal("baseURL leaked in obfuscated file")
	}
	// 空密码即可解（app 自动用内置密钥）
	got2, err := DecryptShare(obf, "")
	if err != nil {
		t.Fatalf("decrypt obfuscated: %v", err)
	}
	if len(got2) != 1 || got2[0].BaseURL != "https://api.groq.com" || got2[0].APIKey != "" {
		t.Fatalf("obfuscated round-trip mismatch: %+v", got2)
	}
	// 预览标记为不需要密码
	prev, err := InspectShare(obf)
	if err != nil {
		t.Fatal(err)
	}
	if prev.NeedPasswd {
		t.Fatal("obfuscated file should not need password")
	}

	// 含 key 但空密码：必须拒绝
	if _, err := mgr.EncryptShare([]string{"groq"}, "", ShareOptions{IncludeAPIKey: true}, "0.0.6", ""); err == nil {
		t.Fatal("expected error when including key without password")
	}
}
