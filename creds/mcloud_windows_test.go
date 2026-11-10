package creds

import (
	"encoding/json"
	"testing"

	"github.com/syndtr/goleveldb/leveldb"
)

// writeFakeLocalStorage 构造一个含单条 localStorage 记录的 leveldb，
// key 用 Chromium 格式 `_file://\x00\x01UserInfo`，value 由调用方给定。
func writeFakeLocalStorage(t *testing.T, val []byte) string {
	t.Helper()
	dir := t.TempDir()
	db, err := leveldb.OpenFile(dir, nil)
	if err != nil {
		t.Fatalf("open fake leveldb: %v", err)
	}
	key := append([]byte("_file://"), 0x00, 0x01)
	key = append(key, []byte("UserInfo")...)
	if err := db.Put(key, val, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dir
}

func TestReadMCloudUserInfoJSON(t *testing.T) {
	ui := `{"account":"13800000000","authToken":"abc|1|RCS|1796892228058|sig","userDomainId":"123456","atExpiretime":2592000}`
	dir := writeFakeLocalStorage(t, append([]byte{0x01}, []byte(ui)...))

	raw, err := readMCloudUserInfoJSON(dir)
	if err != nil {
		t.Fatalf("readMCloudUserInfoJSON: %v", err)
	}
	var info mcLocalUserInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if info.Account != "13800000000" || info.UserDomainID != "123456" || info.AuthToken == "" {
		t.Fatalf("unexpected fields: %+v", info)
	}
	// 过期时间 = 签发毫秒 + 有效期秒*1000
	got := mcTokenIssuedMs(info.AuthToken) + info.AtExpireTime*1000
	want := int64(1796892228058) + int64(2592000)*1000
	if got != want {
		t.Fatalf("expiresAt=%d want %d", got, want)
	}
}

func TestReadMCloudUserInfoJSON_NotFound(t *testing.T) {
	dir := writeFakeLocalStorage(t, append([]byte{0x01}, []byte(`{"foo":"bar"}`)...))
	if _, err := readMCloudUserInfoJSON(dir); err == nil {
		t.Fatal("expected error when no authToken present")
	}
}

func TestMcTokenIssuedMs(t *testing.T) {
	if got := mcTokenIssuedMs("abc|1|RCS|1796892228058|sig"); got != 1796892228058 {
		t.Fatalf("got %d", got)
	}
	if got := mcTokenIssuedMs("bad"); got != 0 {
		t.Fatalf("malformed should be 0, got %d", got)
	}
}

func TestDecodeLocalStorageValue(t *testing.T) {
	if got := decodeLocalStorageValue(append([]byte{0x01}, []byte("hello")...)); got != "hello" {
		t.Fatalf("utf8: got %q", got)
	}
	// "ab" UTF-16LE with 0x00 prefix
	if got := decodeLocalStorageValue([]byte{0x00, 'a', 0x00, 'b', 0x00}); got != "ab" {
		t.Fatalf("utf16: got %q", got)
	}
}
