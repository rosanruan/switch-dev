package creds

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"switchdev/paths"
)

// DevEco 配置
type DevEcoConfig struct {
	Origin           string
	MaasPath         string
	ModelConfigPath  string
	PluginVersion    string
	KEKDir           string
	AuthPath         string
	KVPath           string
	VerifyIntervalMs int
}

func DefaultDevEcoConfig() DevEcoConfig {
	return DevEcoConfig{
		Origin:          "https://cn.devecostudio.huawei.com",
		MaasPath:        "/sse/codeGenie/maas/v2",
		ModelConfigPath: "/codeGenie/modelConfig",
		PluginVersion:   "CLI.0.1.7",
		// DevEco Code 是 Node.js CLI，所有平台（含 Windows）都用字面 XDG 路径，
		// 不能用 XDGConfigDir()（Windows 会映射到 %APPDATA%，导致找不到 token.dek）
		KEKDir:           filepath.Join(paths.LiteralConfigDir(), "deveco"),
		AuthPath:         paths.Resolve("DEVECO_AUTH_PATH", paths.DevEcoAuthCandidates()),
		KVPath:           filepath.Join(paths.LiteralStateDir(), "deveco", "kv.json"),
		VerifyIntervalMs: 600000,
	}
}

// DevEcoCred 运行时凭据
type DevEcoCred struct {
	AccessToken string
	JWTToken    string
	JWTExp      int64 // 毫秒时间戳
	Expires     int64 // access token 过期时间（毫秒）
	FetchedAt   time.Time
	Valid       bool
}

// DevEcoCredManager DevEco 凭据管理器
type DevEcoCredManager struct {
	mu     sync.RWMutex
	cred   *DevEcoCred
	config DevEcoConfig
}

func NewDevEcoCredManager(config DevEcoConfig) *DevEcoCredManager {
	return &DevEcoCredManager{config: config}
}

// Config 返回当前配置
func (m *DevEcoCredManager) Config() DevEcoConfig {
	return m.config
}

// LoadCreds 从本地文件解密出 DevEco access token
func (m *DevEcoCredManager) LoadCreds() (*DevEcoCred, error) {
	kekDir := m.config.KEKDir
	dekPath := filepath.Join(kekDir, "token.dek")

	// 1. 读 DEK 描述文件
	if _, err := os.Stat(dekPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("找不到 DevEco token.dek: %s\n请确认 DevEco Code 已安装并登录过（npm i -g @deveco/deveco-code && deveco auth login）", dekPath)
	}

	dekFileData, err := os.ReadFile(dekPath)
	if err != nil {
		return nil, fmt.Errorf("读取 token.dek 失败: %v", err)
	}

	var dekFile struct {
		KEKID        string `json:"kekId"`
		EncryptedDek string `json:"encryptedDek"`
		IV           string `json:"iv"`
		AuthTag      string `json:"authTag"`
	}
	if err := json.Unmarshal(dekFileData, &dekFile); err != nil {
		return nil, fmt.Errorf("解析 token.dek 失败: %v", err)
	}

	// 2. 读 KEK
	kekPath := filepath.Join(kekDir, "keys", dekFile.KEKID+".bin")
	if _, err := os.Stat(kekPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("找不到 KEK 文件: %s（token.dek 要求 kekId=%s）", kekPath, dekFile.KEKID)
	}

	KEK, err := os.ReadFile(kekPath)
	if err != nil {
		return nil, fmt.Errorf("读取 KEK 失败: %v", err)
	}

	// 3. KEK 解密 DEK
	DEK, err := aesGcmDecrypt(KEK, dekFile.EncryptedDek, dekFile.IV, dekFile.AuthTag)
	if err != nil {
		return nil, fmt.Errorf("解密 DEK 失败: %v", err)
	}

	// DEK 解密后可能是 base64 字符串，需再 decode
	if len(DEK) != 32 {
		decoded, err := base64.StdEncoding.DecodeString(string(DEK))
		if err == nil && len(decoded) == 32 {
			DEK = decoded
		}
	}
	if len(DEK) != 32 {
		return nil, fmt.Errorf("DEK 解密后非 32 字节（实际 %d），无法解密 auth.json", len(DEK))
	}

	// 4. 解密 auth.json 的 deveco.access token
	if _, err := os.Stat(m.config.AuthPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("找不到 DevEco auth.json: %s\n请先运行 deveco auth login 登录", m.config.AuthPath)
	}

	authData, err := os.ReadFile(m.config.AuthPath)
	if err != nil {
		return nil, fmt.Errorf("读取 auth.json 失败: %v", err)
	}

	var auth struct {
		DevEco struct {
			Type   string `json:"type"`
			Access struct {
				Ciphertext string `json:"ciphertext"`
				IV         string `json:"iv"`
				AuthTag    string `json:"authTag"`
			} `json:"access"`
			Refresh struct {
				Ciphertext string `json:"ciphertext"`
			} `json:"refresh"`
			Expires int64 `json:"expires"`
		} `json:"deveco"`
	}
	if err := json.Unmarshal(authData, &auth); err != nil {
		return nil, fmt.Errorf("解析 auth.json 失败: %v", err)
	}

	if auth.DevEco.Type != "oauth" || auth.DevEco.Access.Ciphertext == "" {
		return nil, fmt.Errorf("auth.json 里无有效的 deveco oauth access token，请先运行 deveco auth login")
	}

	accessToken, err := aesGcmDecrypt(DEK, auth.DevEco.Access.Ciphertext, auth.DevEco.Access.IV, auth.DevEco.Access.AuthTag)
	if err != nil {
		return nil, fmt.Errorf("解密 access token 失败: %v", err)
	}

	// 5. 解密 token.enc 拿 jwtToken
	var jwtToken string
	var jwtExp int64

	tokenEncPath := filepath.Join(kekDir, "token.enc")
	if _, err := os.Stat(tokenEncPath); err == nil {
		tokenEncData, err := os.ReadFile(tokenEncPath)
		if err == nil {
			var tokenEnc struct {
				Ciphertext string `json:"ciphertext"`
				IV         string `json:"iv"`
				AuthTag    string `json:"authTag"`
			}
			if json.Unmarshal(tokenEncData, &tokenEnc) == nil {
				jwtBytes, err := aesGcmDecrypt(DEK, tokenEnc.Ciphertext, tokenEnc.IV, tokenEnc.AuthTag)
				if err == nil {
					jwtToken = string(jwtBytes)
					// 解析 JWT payload 的 exp
					parts := strings.Split(jwtToken, ".")
					if len(parts) == 3 {
						// base64url decode payload
						payload, err := base64urlDecode(parts[1])
						if err == nil {
							var payloadObj struct {
								Exp int64 `json:"exp"`
							}
							if json.Unmarshal(payload, &payloadObj) == nil && payloadObj.Exp > 0 {
								jwtExp = payloadObj.Exp * 1000 // 转毫秒
							}
						}
					}
				}
			}
		}
	}

	return &DevEcoCred{
		AccessToken: string(accessToken),
		JWTToken:    jwtToken,
		JWTExp:      jwtExp,
		Expires:     auth.DevEco.Expires,
		FetchedAt:   time.Now(),
		Valid:       false,
	}, nil
}

// EnsureCreds 确保有有效凭据
func (m *DevEcoCredManager) EnsureCreds() (*DevEcoCred, error) {
	m.mu.RLock()
	if m.cred != nil && m.cred.Valid {
		cred := *m.cred
		m.mu.RUnlock()
		return &cred, nil
	}
	m.mu.RUnlock()

	fresh, err := m.LoadCreds()
	if err != nil {
		return nil, err
	}

	// 预校验
	valid, status, err := m.VerifyCreds(fresh)
	if err != nil {
		m.mu.Lock()
		m.cred = fresh
		m.mu.Unlock()
		return nil, fmt.Errorf("DevEco 预校验请求失败: %v", err)
	}

	if valid {
		fresh.Valid = true
		m.mu.Lock()
		m.cred = fresh
		m.mu.Unlock()
		return fresh, nil
	}

	// access token 失效 -> 用 jwtToken 尝试刷新
	fmt.Printf("[switch-dev] DevEco access token 失效（status=%d），尝试用 jwtToken 刷新\n", status)
	rf, err := m.RefreshToken(fresh)
	if err != nil {
		m.mu.Lock()
		m.cred = fresh
		m.mu.Unlock()
		return nil, NewCredentialsError("DevEco access token 刷新失败（%v）。请运行 deveco auth login 重新登录华为账号", err)
	}

	fresh.AccessToken = rf.AccessToken
	fresh.Expires = rf.Expires
	fresh.Valid = true
	m.mu.Lock()
	m.cred = fresh
	m.mu.Unlock()
	return fresh, nil
}

// InvalidateCreds 标记凭据失效
func (m *DevEcoCredManager) InvalidateCreds() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cred != nil {
		m.cred.Valid = false
	}
}

// GetCred 获取当前凭据
func (m *DevEcoCredManager) GetCred() *DevEcoCred {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cred == nil {
		return nil
	}
	cred := *m.cred
	return &cred
}

// CredStatus 返回凭据状态信息
func (m *DevEcoCredManager) CredStatus() *CredStatusInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	info := &CredStatusInfo{
		Source: "auth.json",
	}

	if m.cred != nil {
		info.Valid = m.cred.Valid
		info.LastCheck = m.cred.FetchedAt.Format(time.RFC3339)
		if m.cred.Expires > 0 {
			info.ExpiresAt = time.UnixMilli(m.cred.Expires).Format(time.RFC3339)
		}
		if len(m.cred.AccessToken) > 8 {
			info.KeyPreview = m.cred.AccessToken[:8] + "..."
		}
	} else if _, err := os.Stat(m.config.AuthPath); os.IsNotExist(err) {
		info.Source = "NOT_FOUND"
	}

	// 注入 agent 安装/登录元数据 + Installed 探测
	FillAgentMeta(info, "deveco")
	return info
}

// VerifyCreds 调 GET /codeGenie/modelConfig 验证 access token 有效性
func (m *DevEcoCredManager) VerifyCreds(cred *DevEcoCred) (valid bool, statusCode int, err error) {
	url := fmt.Sprintf("%s%s?localVersion=0&pluginVersion=%s",
		m.config.Origin, m.config.ModelConfigPath, m.config.PluginVersion)

	body, status, err := httpGet(url, map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", cred.AccessToken),
		"Accept":        "application/json",
	})
	if err != nil {
		return false, -1, err
	}

	if status != 200 {
		return false, status, nil
	}

	// 检查返回体是否含模型配置
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, status, nil
	}

	hasModels := resp["body"] != nil || resp["models"] != nil ||
		(resp["data"] != nil && resp["data"].(map[string]interface{})["models"] != nil)

	return hasModels, status, nil
}

// RefreshResult JWT 刷新结果
type RefreshResult struct {
	AccessToken string
	Expires     int64
}

// RefreshToken 用 jwtToken 刷新 access token
func (m *DevEcoCredManager) RefreshToken(cred *DevEcoCred) (*RefreshResult, error) {
	if cred.JWTToken == "" {
		return nil, fmt.Errorf("无 jwtToken（token.enc 缺失或解析失败）")
	}
	if cred.JWTExp > 0 && time.Now().UnixMilli() >= cred.JWTExp {
		return nil, fmt.Errorf("JWT 已过期（%s），需重登", time.UnixMilli(cred.JWTExp).Format(time.RFC3339))
	}

	url := fmt.Sprintf("%s/authrouter/auth/api/jwToken/check", m.config.Origin)
	body, status, err := httpGet(url, map[string]string{
		"refresh":  "true",
		"jwtToken": cred.JWTToken,
		"Accept":   "application/json",
	})
	if err != nil {
		return nil, fmt.Errorf("refresh 请求失败: %v", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("refresh 接口返回 HTTP %d", status)
	}

	var resp struct {
		Status   bool `json:"status"`
		UserInfo struct {
			AccessToken string `json:"accessToken"`
		} `json:"userInfo"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("refresh 接口响应无法解析")
	}
	if !resp.Status || resp.UserInfo.AccessToken == "" {
		return nil, fmt.Errorf("refresh 接口返回无效响应（status=%v）", resp.Status)
	}

	return &RefreshResult{
		AccessToken: resp.UserInfo.AccessToken,
		Expires:     time.Now().Add(30 * time.Minute).UnixMilli(),
	}, nil
}

// ReadImprovementEnabled 读 DevEco 隐私开关
func (m *DevEcoCredManager) ReadImprovementEnabled() bool {
	if _, err := os.Stat(m.config.KVPath); os.IsNotExist(err) {
		return true
	}
	data, err := os.ReadFile(m.config.KVPath)
	if err != nil {
		return true
	}
	var kv map[string]interface{}
	if err := json.Unmarshal(data, &kv); err != nil {
		return true
	}
	if v, ok := kv["deveco_tool_improvement_enabled"]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return true
}

// ====== 加密工具函数 ======

// aesGcmDecrypt AES-256-GCM 解密
func aesGcmDecrypt(key []byte, ciphertextB64, ivB64, authTagB64 string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("创建 AES cipher 失败: %v", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("创建 GCM 失败: %v", err)
	}

	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("base64 解码 ciphertext 失败: %v", err)
	}

	iv, err := base64.StdEncoding.DecodeString(ivB64)
	if err != nil {
		return nil, fmt.Errorf("base64 解码 iv 失败: %v", err)
	}

	authTag, err := base64.StdEncoding.DecodeString(authTagB64)
	if err != nil {
		return nil, fmt.Errorf("base64 解码 authTag 失败: %v", err)
	}

	// GCM 的 nonce 大小
	if len(iv) != gcm.NonceSize() {
		return nil, fmt.Errorf("IV 大小不匹配：期望 %d，实际 %d", gcm.NonceSize(), len(iv))
	}

	// ciphertext + authTag 拼接后传给 Open
	sealed := append(ciphertext, authTag...)
	return gcm.Open(nil, iv, sealed, nil)
}

// base64urlDecode base64url 解码（JWT payload 使用）
func base64urlDecode(s string) ([]byte, error) {
	// 补 padding
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}

// randUUID 生成 32 位无横杠 UUID（DevEco Chat-Id 需要）
func randUUID32() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%08x%04x%04x%04x%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
