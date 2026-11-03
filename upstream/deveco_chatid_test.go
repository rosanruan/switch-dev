package upstream

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"switchdev/creds"
)

func TestDevEcoChatIDStablePerConversation(t *testing.T) {
	ctxA := WithConversationKey(context.Background(), "session-a")
	ctxB := WithConversationKey(context.Background(), "session-b")

	first := devEcoChatID(ctxA)
	if len(first) != 32 {
		t.Fatalf("len(Chat-Id) = %d, want 32", len(first))
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("Chat-Id is not hex: %q", first)
	}
	if got := devEcoChatID(ctxA); got != first {
		t.Fatalf("same conversation Chat-Id changed: %q -> %q", first, got)
	}
	if got := devEcoChatID(ctxB); got == first {
		t.Fatalf("different conversations share Chat-Id %q", got)
	}
}

func TestDevEcoChatIDWithoutConversationRemainsRandom(t *testing.T) {
	first := devEcoChatID(context.Background())
	second := devEcoChatID(context.Background())
	if len(first) != 32 || len(second) != 32 {
		t.Fatalf("fallback IDs must be 32 characters: %q, %q", first, second)
	}
	if first == second {
		t.Fatalf("fallback Chat-Id was reused: %q", first)
	}
}

func TestDevEcoCallPathsUseConversationChatID(t *testing.T) {
	ids := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids <- r.Header.Get("Chat-Id")
		if strings.Contains(r.URL.Path, "/no-stream/") {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, firstBurst("ok")+"data: [DONE]\n\n")
	}))
	defer srv.Close()

	mgr := creds.NewDevEcoCredManager(creds.DevEcoConfig{
		Origin:   srv.URL,
		MaasPath: "/maas",
		KVPath:   filepath.Join(t.TempDir(), "kv.json"),
	})
	up := NewDevEcoUpstream(mgr)
	ctx := WithConversationKey(context.Background(), "session-a")
	cred := &creds.DevEcoCred{AccessToken: "test-token"}
	body := []byte(`{"model":"GLM-5.1","messages":[]}`)

	if _, err := up.doCall(ctx, body, cred); err != nil {
		t.Fatalf("doCall: %v", err)
	}
	sr, err := up.doCallStream(ctx, body, cred)
	if err != nil {
		t.Fatalf("doCallStream: %v", err)
	}
	sr.Body.Close()

	nonStreamID := <-ids
	streamID := <-ids
	want := devEcoChatID(ctx)
	if nonStreamID != want {
		t.Fatalf("non-stream Chat-Id = %q, want %q", nonStreamID, want)
	}
	if streamID != want {
		t.Fatalf("stream Chat-Id = %q, want %q", streamID, want)
	}
}
