package upstream

import (
	"crypto/rand"
	"math/big"
)

// randString 生成指定长度的随机字符串（字母数字）
func randString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		b[i] = letters[n.Int64()]
	}
	return string(b)
}
