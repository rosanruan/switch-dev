//go:build windows

package keystore

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// winBackend 用 Windows DPAPI（Crypt32.dll）加密存储密码，
// 密文绑定当前 Windows 用户账户，换用户/换机器无法解密。
type winBackend struct{}

var platformBackend backend = winBackend{}

// dataBlob 对应 DPAPI 的 DATA_BLOB 结构
type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(d []byte) *dataBlob {
	if len(d) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{pbData: &d[0], cbData: uint32(len(d))}
}

func (b *dataBlob) toBytes() []byte {
	if b.cbData == 0 {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, (*[1 << 30]byte)(unsafe.Pointer(b.pbData))[:b.cbData])
	return out
}

const cryptProtectUIFORBIDDEN = 0x1

var (
	dllCrypt32    = windows.NewLazySystemDLL("Crypt32.dll")
	procProtect   = dllCrypt32.NewProc("CryptProtectData")
	procUnprotect = dllCrypt32.NewProc("CryptUnprotectData")
)

func (winBackend) set(_, account, value string) error {
	enc, err := dpapiEncrypt([]byte(value))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(secretDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(secretPath(account), []byte(base64.StdEncoding.EncodeToString(enc)), 0o600)
}

func (winBackend) get(_, account string) (string, error) {
	b, err := os.ReadFile(secretPath(account))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	raw := strings.TrimSpace(string(b))
	if raw == "" {
		return "", nil
	}

	// 优先按 DPAPI 密文解密
	if enc, err := base64.StdEncoding.DecodeString(raw); err == nil {
		if plain, err := dpapiDecrypt(enc); err == nil {
			return string(plain), nil
		}
	}
	// 兼容旧版明文存储：直接返回
	return raw, nil
}

func (winBackend) delete(_, account string) error {
	if err := os.Remove(secretPath(account)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// recover 单存储平台：读取并校验密码是否正确。
func (winBackend) recover(service, account string, valid func(string) bool) string {
	v, err := winBackend{}.get(service, account)
	if err != nil || v == "" {
		return ""
	}
	if valid(v) {
		return v
	}
	return ""
}

// dpapiEncrypt 用 CryptProtectData 加密（绑定当前用户）
func dpapiEncrypt(data []byte) ([]byte, error) {
	var outBlob dataBlob
	inBlob := newBlob(data)
	r, _, err := procProtect.Call(
		uintptr(unsafe.Pointer(inBlob)),
		0,
		0,
		0,
		0,
		uintptr(cryptProtectUIFORBIDDEN),
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if r == 0 {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(outBlob.pbData)))
	return outBlob.toBytes(), nil
}

// dpapiDecrypt 用 CryptUnprotectData 解密（仅当前用户可解）
func dpapiDecrypt(data []byte) ([]byte, error) {
	var outBlob dataBlob
	inBlob := newBlob(data)
	r, _, err := procUnprotect.Call(
		uintptr(unsafe.Pointer(inBlob)),
		0,
		0,
		0,
		0,
		uintptr(cryptProtectUIFORBIDDEN),
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if r == 0 {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(outBlob.pbData)))
	return outBlob.toBytes(), nil
}

func secretDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "switch-dev", "secrets")
	}
	return filepath.Join(os.Getenv("APPDATA"), "switch-dev", "secrets")
}

func secretPath(account string) string {
	s := strings.ReplaceAll(account, string(filepath.Separator), "_")
	s = strings.ReplaceAll(s, "..", "_")
	return filepath.Join(secretDir(), s+".secret")
}
