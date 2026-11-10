package providerapi

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// ── 二进制容器格式（.sds，大端序）──
//
//	偏移 长度 字段
//	0    2    magic = 'S','D' (0x53 0x44)
//	2    1    version（当前 1）
//	3    1    flags（bit0 = encrypted）
//	4    1    kdfAlgo（1 = argon2id）
//	5    1    cipherAlgo（1 = AES-256-GCM）
//	6    2    saltLen（uint16）
//	8    N    salt
//	..   2    ivLen（uint16）
//	..   N    iv（GCM nonce，通常 12）
//	..   2    tagLen（uint16）
//	..   N    authTag（GCM tag，通常 16）
//	..   4    ciphertextLen（uint32）
//	..   N    ciphertext（AES-GCM 加密的 payload JSON）
const (
	shareVersion    = 1
	shareMagic0     = byte('S') // 0x53
	shareMagic1     = byte('D') // 0x44
	flagEncrypted   = byte(1 << 0)
	kdfArgon2id     = byte(1) // 密码派生密钥（真加密）
	kdfEmbedded     = byte(2) // 内置固定密钥（无密码混淆，非真加密）
	cipherAES256GCM = byte(1)
)

// embeddedObfuscationKey 是"无需密码"模式用的固定 AES 密钥。
// 注意：它随程序分发，只能防止文件被肉眼直接读取（混淆），不能抵御逆向。
// 含 API Key 的导出必须用密码模式（kdfArgon2id）。
var embeddedObfuscationKey = []byte{
	0x3a, 0x71, 0xc4, 0x9f, 0x2d, 0xe8, 0x4b, 0x06,
	0x85, 0xbf, 0x19, 0xd7, 0x60, 0xf2, 0xa3, 0x5c,
	0x94, 0x0e, 0x7b, 0xd1, 0x48, 0xaf, 0x26, 0xc9,
	0x57, 0x80, 0xeb, 0x13, 0x6d, 0xb4, 0xf8, 0x32,
}

// argon2id 参数（抗离线暴力；旧文件按文件头里存的参数解析）
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32 // AES-256
	saltLen      = 16
)

// ShareHeader 解析后的文件头信息（不解密即可读）
type ShareHeader struct {
	Version    uint8
	Encrypted  bool
	KDFAlgo    byte
	CipherAlgo byte
	Salt       []byte
	IV         []byte
	AuthTag    []byte
}

// SharePayload 解密后的实际内容（全部在密文里）
type SharePayload struct {
	AppVersion string          `json:"appVersion,omitempty"`
	ExportedAt string          `json:"exportedAt,omitempty"`
	Providers  []ShareProvider `json:"providers"`
}

// ShareProvider 分享的单个供应商（只含可分享字段，不含本地运行时状态）
type ShareProvider struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	BaseURL      string          `json:"baseURL"`
	GetAPIKeyURL string          `json:"getAPIKeyURL"`
	Protocol     string          `json:"protocol"`
	Custom       bool            `json:"custom"`
	APIKey       string          `json:"apiKey"`
	Models       []ProviderModel `json:"models"`
}

// SharePreview 不解密时能看到的元信息（加密文件看不到供应商）
type SharePreview struct {
	Version    int  `json:"version"`
	Encrypted  bool `json:"encrypted"`
	NeedPasswd bool `json:"needPasswd"` // kdf=argon2id 需要密码；kdf=embedded 无需密码
}

// GenerateSharePassword 生成一次性分享密码（6 位字母+数字，去掉易混字符 0/O/l/1/I）。
// 文件本身用 argon2id 派生密钥，暴力破解成本高，6 位足够短场景使用。
// 用 crypto/rand，密码学安全。
func GenerateSharePassword() (string, error) {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	b := make([]byte, 6)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	out := make([]byte, 6)
	for i, v := range b {
		out[i] = charset[int(v)%len(charset)]
	}
	return string(out), nil
}

// ShareOptions 导出选项
type ShareOptions struct {
	IncludeAPIKey bool // 是否包含 API Key
	// Encrypt 已废弃：文件始终加密；保留字段以兼容旧调用。
	Encrypt bool
}

// EncryptShare 把指定供应商导出成 .sds 二进制字节。
//   - 含 API Key（IncludeAPIKey=true）：必须提供密码，argon2id 派生密钥（真加密）。
//   - 不含 Key + 提供密码：同样 argon2id 真加密。
//   - 不含 Key + 空密码：用内置固定密钥 AES-GCM 混淆（文件为密文，但任何持有本程序者可解）。
func (m *Manager) EncryptShare(ids []string, password string, opts ShareOptions, appVersion, exportedAt string) ([]byte, error) {
	if len(ids) == 0 {
		return nil, errors.New("请至少选择一个供应商")
	}
	if opts.IncludeAPIKey && strings.TrimSpace(password) == "" {
		return nil, errors.New("包含 API Key 时必须设置密码")
	}

	m.mu.RLock()
	providers := make([]ShareProvider, 0, len(ids))
	for _, id := range ids {
		p, ok := m.config.Providers[id]
		if !ok {
			m.mu.RUnlock()
			return nil, fmt.Errorf("供应商不存在: %s", id)
		}
		cp := cloneProvider(p)
		apiKey := cp.APIKey
		if !opts.IncludeAPIKey {
			apiKey = ""
		}
		providers = append(providers, ShareProvider{
			ID:           cp.ID,
			Name:         cp.Name,
			BaseURL:      cp.BaseURL,
			GetAPIKeyURL: cp.GetAPIKeyURL,
			Protocol:     cp.Protocol,
			Custom:       cp.Custom,
			APIKey:       apiKey,
			Models:       cp.Models,
		})
	}
	m.mu.RUnlock()

	plain, err := json.Marshal(SharePayload{
		AppVersion: appVersion,
		ExportedAt: exportedAt,
		Providers:  providers,
	})
	if err != nil {
		return nil, err
	}

	// 含密码用 argon2id 真加密；无密码（仅不含 key 时允许）用内置密钥混淆
	usePassword := strings.TrimSpace(password) != ""
	var key, salt []byte
	kdfAlgo := kdfEmbedded
	if usePassword {
		kdfAlgo = kdfArgon2id
		salt = make([]byte, saltLen)
		if _, err := io.ReadFull(rand.Reader, salt); err != nil {
			return nil, err
		}
		key = argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	} else {
		key = embeddedObfuscationKey
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, err
	}
	// Seal 输出 ciphertext||tag
	sealed := gcm.Seal(nil, iv, plain, nil)
	tag := sealed[len(sealed)-gcm.Overhead():]
	ct := sealed[:len(sealed)-gcm.Overhead()]

	return encodeShareBinary(kdfAlgo, salt, iv, tag, ct), nil
}

// encodeShareBinary 按二进制布局封装（始终为密文；kdfAlgo 区分密码派生育是内置密钥）。
func encodeShareBinary(kdfAlgo byte, salt, iv, tag, ciphertext []byte) []byte {
	buf := make([]byte, 0, 12+len(salt)+len(iv)+len(tag)+len(ciphertext))
	buf = append(buf, shareMagic0, shareMagic1)
	buf = append(buf, shareVersion)
	buf = append(buf, flagEncrypted)
	buf = append(buf, kdfAlgo, cipherAES256GCM)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(salt)))
	buf = append(buf, salt...)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(iv)))
	buf = append(buf, iv...)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(tag)))
	buf = append(buf, tag...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(ciphertext)))
	buf = append(buf, ciphertext...)
	return buf
}

// InspectShare 只读文件头，返回版本与是否加密（不解密、不返回供应商）。
func InspectShare(data []byte) (*SharePreview, error) {
	h, rest, err := parseShareHeader(data)
	if err != nil {
		return nil, err
	}
	_ = rest // 密文部分在解密时才需要
	return &SharePreview{
		Version:    int(h.Version),
		Encrypted:  h.Encrypted,
		NeedPasswd: h.KDFAlgo == kdfArgon2id,
	}, nil
}

// DecryptShare 用密码解密 .sds，返回供应商列表（不写入配置）。
func DecryptShare(data []byte, password string) ([]ShareProvider, error) {
	h, ciphertext, err := parseShareHeader(data)
	if err != nil {
		return nil, err
	}
	if !h.Encrypted {
		// 当前版本总是加密；保留未加密直读路径以备将来
		var p SharePayload
		if err := json.Unmarshal(ciphertext, &p); err != nil {
			return nil, fmt.Errorf("解析内容失败: %w", err)
		}
		return p.Providers, nil
	}
	if h.CipherAlgo != cipherAES256GCM {
		return nil, fmt.Errorf("不支持的加密算法（cipher=%d），请升级应用", h.CipherAlgo)
	}
	if len(h.IV) == 0 || len(h.AuthTag) == 0 {
		return nil, errors.New("文件头缺少 IV/AuthTag")
	}

	var key []byte
	switch h.KDFAlgo {
	case kdfArgon2id:
		// 密码模式：忽略空密码
		if strings.TrimSpace(password) == "" {
			return nil, errors.New("该文件已加密，请输入分享密码")
		}
		key = argon2.IDKey([]byte(password), h.Salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	case kdfEmbedded:
		// 无密码模式：内置固定密钥，任何持有本程序者可解（混淆）
		key = embeddedObfuscationKey
	default:
		return nil, fmt.Errorf("不支持的密钥派生算法（kdf=%d），请升级应用", h.KDFAlgo)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(h.IV) != gcm.NonceSize() {
		return nil, fmt.Errorf("IV 长度不符：期望 %d，实际 %d", gcm.NonceSize(), len(h.IV))
	}
	sealed := append(ciphertext, h.AuthTag...)
	plain, err := gcm.Open(nil, h.IV, sealed, nil)
	if err != nil {
		if h.KDFAlgo == kdfArgon2id {
			return nil, errors.New("解密失败：密码错误或文件已损坏")
		}
		return nil, errors.New("解密失败：文件已损坏")
	}
	var p SharePayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, fmt.Errorf("解析内容失败: %w", err)
	}
	return p.Providers, nil
}

// parseShareHeader 校验 magic/version 并按布局读出文件头与密文。
func parseShareHeader(data []byte) (*ShareHeader, []byte, error) {
	if len(data) < 8 {
		return nil, nil, errors.New("不是有效的 .sds 文件（过短）")
	}
	if data[0] != shareMagic0 || data[1] != shareMagic1 {
		return nil, nil, errors.New("不是有效的 .sds 文件（缺少 SD 标识）")
	}
	h := &ShareHeader{
		Version:    data[2],
		Encrypted:  data[3]&flagEncrypted != 0,
		KDFAlgo:    data[4],
		CipherAlgo: data[5],
	}
	if h.Version > shareVersion {
		return nil, nil, fmt.Errorf("分享文件来自更新版本（v%d），请升级应用后再导入", h.Version)
	}

	pos := 6
	read16 := func() ([]byte, error) {
		if pos+2 > len(data) {
			return nil, errors.New("文件已损坏：长度字段越界")
		}
		n := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		if pos+n > len(data) {
			return nil, errors.New("文件已损坏：字段长度越界")
		}
		var b []byte
		if n > 0 {
			b = data[pos : pos+n]
		}
		pos += n
		return b, nil
	}

	var err error
	if h.Salt, err = read16(); err != nil {
		return nil, nil, err
	}
	if h.IV, err = read16(); err != nil {
		return nil, nil, err
	}
	if h.AuthTag, err = read16(); err != nil {
		return nil, nil, err
	}
	if pos+4 > len(data) {
		return nil, nil, errors.New("文件已损坏：缺少密文长度")
	}
	ctLen := int(binary.BigEndian.Uint32(data[pos : pos+4]))
	pos += 4
	if pos+ctLen != len(data) {
		return nil, nil, errors.New("文件已损坏：密文长度不匹配")
	}
	return h, data[pos : pos+ctLen], nil
}

// ImportStrategy 单个供应商的导入冲突处理
type ImportStrategy string

const (
	ImportOverwrite ImportStrategy = "overwrite" // 覆盖已存在的同 id 供应商
	ImportSkip      ImportStrategy = "skip"      // 跳过（不导入）
	ImportRename    ImportStrategy = "rename"    // 以新 id 导入（保留双方）
)

// ImportItem 一条导入指令（前端预览/解决冲突后下发）
type ImportItem struct {
	Provider ShareProvider  `json:"provider"`
	Strategy ImportStrategy `json:"strategy"`
	NewID    string         `json:"newId"` // rename 时使用；为空则自动加 -2/-3…
}

// ImportProviders 按指令导入供应商。overwrite 整包替换；rename 用新 id 追加；
// 不带 APIKey 的导入，所有模型 verified 重置为 false（需接收方自行评测）。
func (m *Manager) ImportProviders(items []ImportItem) error {
	m.mu.Lock()
	if m.config.Providers == nil {
		m.config.Providers = map[string]*ProviderConfig{}
	}
	for _, it := range items {
		sp := it.Provider
		targetID := strings.TrimSpace(sp.ID)
		switch it.Strategy {
		case ImportSkip:
			continue
		case ImportRename:
			// 用户无需输入 id，基于原 id 加 6 位随机后缀区分
			targetID = m.uniqueID(generateRenameID(sp.ID))
		case ImportOverwrite:
			// 同 id 覆盖
		default:
			m.mu.Unlock()
			return fmt.Errorf("未知的导入策略: %s", it.Strategy)
		}
		models := make([]ProviderModel, len(sp.Models))
		hasKey := strings.TrimSpace(sp.APIKey) != ""
		for i, mo := range sp.Models {
			mo.ID = strings.TrimSpace(mo.ID)
			if !hasKey {
				mo.Verified = false
				mo.Healthy = false
				mo.FailCount = 0
				mo.TPS = 0
			}
			models[i] = mo
		}
		m.config.Providers[targetID] = &ProviderConfig{
			ID:           targetID,
			Name:         sp.Name,
			BaseURL:      sp.BaseURL,
			APIKey:       sp.APIKey,
			GetAPIKeyURL: sp.GetAPIKeyURL,
			Protocol:     sp.Protocol,
			Custom:       sp.Custom,
			Imported:     true, // 来自分享文件导入
			Verified:     hasKey && len(models) > 0,
			Models:       models,
		}
	}
	m.mu.Unlock()
	// 解锁后再写盘（Save 内部会取读锁，不能在持有写锁时调用，否则自死锁）
	return m.Save()
}

// uniqueID 在现有 id 基础上追加 -2/-3… 直到不冲突
func (m *Manager) uniqueID(base string) string {
	id := base
	for i := 2; ; i++ {
		if _, exists := m.config.Providers[id]; !exists {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
}

// generateRenameID 基于原 id 生成 "原id-随机6位" 的新 id（小写字母数字）。
func generateRenameID(original string) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		// crypto/rand 极少失败；兜底用时间戳保证唯一
		return fmt.Sprintf("%s-%d", original, time.Now().UnixNano()%1000000)
	}
	for i, v := range b {
		b[i] = charset[int(v)%len(charset)]
	}
	return original + "-" + string(b)
}
