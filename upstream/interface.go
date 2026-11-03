package upstream

import (
	"context"
	"io"

	"switchdev/creds"
)

// Upstream 上游适配器统一接口
type Upstream interface {
	// Call 调用上游（传入 OpenAI 格式 body 的 JSON 字节），返回上游原始响应
	Call(ctx context.Context, body []byte) (*Response, error)
	// VerifyCreds 预检凭据有效性
	VerifyCreds(ctx context.Context) (*VerifyResult, error)
	// EnsureCreds 确保凭据有效（含自动恢复）
	EnsureCreds(ctx context.Context) error
	// InvalidateCreds 标记凭据失效
	InvalidateCreds()
	// CredStatus 当前凭据状态
	CredStatus() *creds.CredStatusInfo
	// HasValidCreds 凭据当前是否有效（快速检查，不触发网络请求）
	HasValidCreds() bool
	// FetchModels 拉取上游可用模型列表（实时，失败返回 error 由调用方回退本地白名单）
	FetchModels(ctx context.Context) ([]FetchedModel, error)
	// Name 上游名称
	Name() string
}

// Response 上游响应
type Response struct {
	StatusCode int
	Body       []byte
	ReqID      string
}

// StreamResponse 流式响应
// 200 时 Body 是 SSE 流（由调用方在转发结束后 Close）；
// 非 200 时 Body 是已读入的错误体 reader，调用方可读后 Close
type StreamResponse struct {
	StatusCode int
	Body       io.ReadCloser
	ReqID      string
}

// StreamCaller 可选接口：支持真流式调用（上游 stream:true，返回 SSE 流）
// 未实现该接口的上游走伪流式（Call + 代理拆 SSE）
type StreamCaller interface {
	CallStream(ctx context.Context, body []byte) (*StreamResponse, error)
}

type conversationContextKey struct{}

func WithConversationKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, conversationContextKey{}, key)
}

func ConversationKey(ctx context.Context) string {
	key, _ := ctx.Value(conversationContextKey{}).(string)
	return key
}

// VerifyResult 预检结果
type VerifyResult struct {
	Valid  bool
	Code   int
	Status int
}