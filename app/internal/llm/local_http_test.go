package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
)

func newLocalAgainst(srv *httptest.Server) *Local {
	return NewLocal(config.LocalConfig{
		Endpoint:  srv.URL + "/v1",
		Model:     "test-model",
		APIKeyEnv: "",
	})
}

func TestLocal_Chat_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "hi back"}}],
			"usage": {"prompt_tokens": 7, "completion_tokens": 3}
		}`))
	}))
	defer srv.Close()

	l := newLocalAgainst(srv)
	resp, err := l.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi back" {
		t.Errorf("Content = %q, want 'hi back'", resp.Content)
	}
	if resp.PromptTokens != 7 || resp.OutputTokens != 3 {
		t.Errorf("usage tokens not parsed: prompt=%d output=%d", resp.PromptTokens, resp.OutputTokens)
	}
}

func TestLocal_Chat_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal model error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	l := newLocalAgainst(srv)
	_, err := l.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("expected HTTP 500 to surface as error")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %v, want 'HTTP 500'", err)
	}
}

func TestLocal_Chat_HTTP400_BodyTruncated(t *testing.T) {
	bigBody := strings.Repeat("x", 2000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(bigBody))
	}))
	defer srv.Close()

	l := newLocalAgainst(srv)
	_, err := l.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("expected HTTP 400 error")
	}
	// doRequest truncates to ~512 chars, so the error message must be
	// shorter than the source body.
	if len(err.Error()) >= len(bigBody) {
		t.Errorf("error body should be truncated; len = %d", len(err.Error()))
	}
}

func TestLocal_Chat_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()

	l := newLocalAgainst(srv)
	_, err := l.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse response") {
		t.Errorf("error = %v, want 'parse response'", err)
	}
}

func TestLocal_Chat_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [], "usage": {}}`))
	}))
	defer srv.Close()

	l := newLocalAgainst(srv)
	resp, err := l.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("empty choices should not error: %v", err)
	}
	if resp.Content != "" {
		t.Errorf("Content = %q, want empty", resp.Content)
	}
}

func TestLocal_Chat_ToolCallsParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"message": {
					"role": "assistant",
					"content": "",
					"tool_calls": [{
						"id": "tc-1",
						"type": "function",
						"function": {"name": "list-files", "arguments": "{\"path\":\"/tmp\"}"}
					}]
				}
			}],
			"usage": {}
		}`))
	}))
	defer srv.Close()

	l := newLocalAgainst(srv)
	resp, err := l.Chat(context.Background(), []Message{{Role: RoleUser, Content: "list /tmp"}}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "tc-1" || tc.Name != "list-files" || !strings.Contains(tc.Arguments, "/tmp") {
		t.Errorf("tool call mis-parsed: %+v", tc)
	}
}

func TestLocal_doRequest_AuthorizationHeader(t *testing.T) {
	t.Setenv("LOCAL_TEST_KEY", "secret-token")
	receivedAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{}}`))
	}))
	defer srv.Close()

	l := NewLocal(config.LocalConfig{
		Endpoint:  srv.URL + "/v1",
		Model:     "test-model",
		APIKeyEnv: "LOCAL_TEST_KEY",
	})
	_, err := l.Chat(context.Background(), []Message{{Role: RoleUser, Content: "x"}}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if receivedAuth != "Bearer secret-token" {
		t.Errorf("Authorization header = %q, want 'Bearer secret-token'", receivedAuth)
	}
}

func TestLocal_doRequest_NoAuthorizationWhenKeyEmpty(t *testing.T) {
	t.Setenv("LOCAL_TEST_KEY_MISSING", "")
	receivedAuth := "<unset-sentinel>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{}}`))
	}))
	defer srv.Close()

	l := NewLocal(config.LocalConfig{
		Endpoint:  srv.URL + "/v1",
		Model:     "test-model",
		APIKeyEnv: "LOCAL_TEST_KEY_MISSING",
	})
	if _, err := l.Chat(context.Background(), []Message{{Role: RoleUser, Content: "x"}}, nil); err != nil {
		t.Fatal(err)
	}
	if receivedAuth != "" {
		t.Errorf("expected no Authorization header, got %q", receivedAuth)
	}
}

func TestLocal_ChatStream_AccumulatesDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeChunk := func(s string) {
			_, _ = w.Write([]byte("data: " + s + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeChunk(`{"choices":[{"delta":{"content":"Hel"}}]}`)
		writeChunk(`{"choices":[{"delta":{"content":"lo"}}]}`)
		writeChunk(`{"choices":[{"delta":{"content":"!"}}]}`)
		writeChunk("[DONE]")
	}))
	defer srv.Close()

	var streamed strings.Builder
	doneSeen := false
	cb := func(token string, calls []ToolCall, done bool) {
		streamed.WriteString(token)
		if done {
			doneSeen = true
		}
	}
	l := newLocalAgainst(srv)
	resp, err := l.ChatStream(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil, cb)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if streamed.String() != "Hello!" {
		t.Errorf("streamed tokens = %q, want 'Hello!'", streamed.String())
	}
	if !doneSeen {
		t.Error("done callback not invoked")
	}
	if resp.Content != "Hello!" {
		t.Errorf("Response.Content = %q, want 'Hello!'", resp.Content)
	}
}

func TestLocal_ChatStream_ToolCallReassembly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeChunk := func(s string) {
			_, _ = w.Write([]byte("data: " + s + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Tool call streamed across multiple chunks (id+name first, args split).
		writeChunk(`{"choices":[{"delta":{"tool_calls":[{"id":"tc1","type":"function","function":{"name":"list-files","arguments":"{\"pa"}}]}}]}`)
		writeChunk(`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"th\":\"/tmp\"}"}}]}}]}`)
		writeChunk("[DONE]")
	}))
	defer srv.Close()

	l := newLocalAgainst(srv)
	resp, err := l.ChatStream(context.Background(), []Message{{Role: RoleUser, Content: "list"}}, nil, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "list-files" || !strings.Contains(tc.Arguments, "/tmp") {
		t.Errorf("tool call mis-assembled: %+v", tc)
	}
}

func TestLocal_ChatStream_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	l := newLocalAgainst(srv)
	_, err := l.ChatStream(context.Background(), []Message{{Role: RoleUser, Content: "x"}}, nil, nil)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestLocal_doRequest_RespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't reply; rely on context cancellation.
		<-r.Context().Done()
	}))
	defer srv.Close()

	l := newLocalAgainst(srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediate cancel
	_, err := l.Chat(ctx, []Message{{Role: RoleUser, Content: "x"}}, nil)
	if err == nil {
		t.Fatal("expected error after context cancel")
	}
}
