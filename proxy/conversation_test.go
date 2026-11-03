package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"switchdev/upstream"
)

func TestWithConversationContext(t *testing.T) {
	const sessionUserID = "user_123_account__session_abc"

	tests := []struct {
		name           string
		header         string
		metadataUserID string
		want           string
	}{
		{name: "explicit header", header: "client-chat-id", metadataUserID: sessionUserID, want: "client-chat-id"},
		{name: "Claude Code session metadata", metadataUserID: sessionUserID, want: sessionUserID},
		{name: "account only metadata", metadataUserID: "user_123_account"},
		{name: "missing identity"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			if tt.header != "" {
				req.Header.Set("Chat-Id", tt.header)
			}
			ctx := withConversationContext(req, tt.metadataUserID)
			if got := upstream.ConversationKey(ctx); got != tt.want {
				t.Fatalf("ConversationKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAnthropicRequestPreservesSessionMetadata(t *testing.T) {
	const userID = "user_123_account__session_abc"
	var body AnthropicRequest
	if err := json.Unmarshal([]byte(`{"model":"glm-5.1","metadata":{"user_id":"`+userID+`"}}`), &body); err != nil {
		t.Fatal(err)
	}
	if body.Metadata == nil || body.Metadata.UserID != userID {
		t.Fatalf("Metadata = %+v, want user_id %q", body.Metadata, userID)
	}

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	ctx := withConversationContext(req, body.Metadata.UserID)
	if got := upstream.ConversationKey(ctx); got != userID {
		t.Fatalf("ConversationKey() = %q, want %q", got, userID)
	}
}
