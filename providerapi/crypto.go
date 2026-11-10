package providerapi

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

// argon2id KDF 参数（主密码/恢复码派生 KEK）
const (
	kdfTime    = 3
	kdfMemory  = 64 * 1024 // 64 MiB
	kdfThreads = 4
	kdfKeyLen  = 32 // AES-256
	kdfSaltLen = 16
)

// sealed 是 AES-256-GCM 加密后的字段（apiKey / wrappedDEK 都用这个结构）
type sealed struct {
	Ciphertext string `json:"ciphertext"`
	IV         string `json:"iv"`
	AuthTag    string `json:"authTag"`
}

// Sealed 是 sealed 的导出别名，供外部包使用
type Sealed = sealed

// AesGCMSeal 导出包装，供外部包加密字段
func AesGCMSeal(key, plaintext []byte) (*Sealed, error) {
	return aesGCMSeal(key, plaintext)
}

// AesGCMOpen 导出包装，供外部包解密字段
func AesGCMOpen(key []byte, s *Sealed) ([]byte, error) {
	return aesGCMOpen(key, s)
}

// deriveKey 用 argon2id 从密码 + salt 派生 32B 密钥
func deriveKey(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, kdfTime, kdfMemory, kdfThreads, kdfKeyLen)
}

// aesGCMSeal 用 key 加密 plaintext，返回密文/iv/tag（均为新随机 IV）
func aesGCMSeal(key, plaintext []byte) (*sealed, error) {
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
	ct := gcm.Seal(nil, iv, plaintext, nil)
	tag := ct[len(ct)-gcm.Overhead():]
	ciphertext := ct[:len(ct)-gcm.Overhead()]
	return &sealed{
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
		IV:         base64.StdEncoding.EncodeToString(iv),
		AuthTag:    base64.StdEncoding.EncodeToString(tag),
	}, nil
}

// aesGCMOpen 用 key 解密封装字段
func aesGCMOpen(key []byte, s *sealed) ([]byte, error) {
	if s == nil {
		return nil, errors.New("密文为空")
	}
	ct, err := base64.StdEncoding.DecodeString(s.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("ciphertext: %w", err)
	}
	iv, err := base64.StdEncoding.DecodeString(s.IV)
	if err != nil {
		return nil, fmt.Errorf("iv: %w", err)
	}
	tag, err := base64.StdEncoding.DecodeString(s.AuthTag)
	if err != nil {
		return nil, fmt.Errorf("authTag: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(iv) != gcm.NonceSize() {
		return nil, fmt.Errorf("IV 长度不符: 期望 %d，实际 %d", gcm.NonceSize(), len(iv))
	}
	sealed := append(ct, tag...)
	return gcm.Open(nil, iv, sealed, nil)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = io.ReadFull(rand.Reader, b)
	return b
}

// randomRecoveryCode 生成 24 位恢复码（去易混字符），每 6 位加分隔符方便抄写
func randomRecoveryCode() string {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	raw := make([]byte, 24)
	b := make([]byte, 24)
	_, _ = io.ReadFull(rand.Reader, raw)
	for i, v := range raw {
		b[i] = charset[int(v)%len(charset)]
	}
	return fmt.Sprintf("%s-%s-%s-%s", string(b[0:6]), string(b[6:12]), string(b[12:18]), string(b[18:24]))
}
