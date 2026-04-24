package mcp

import (
	"encoding/json"
	"testing"
)

func TestRequestMarshal(t *testing.T) {
	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/list",
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed Request
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Method != "tools/list" {
		t.Errorf("method = %v, want tools/list", parsed.Method)
	}
}

func TestResponseWithError(t *testing.T) {
	data := `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"Invalid Request"}}`
	var resp Response
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if resp.Error.Code != -32600 {
		t.Errorf("error code = %d, want -32600", resp.Error.Code)
	}
}

func TestResponseWithResult(t *testing.T) {
	data := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"test","description":"A test tool"}]}}`
	var resp Response
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	var result struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Tools) != 1 {
		t.Fatalf("tools count = %d, want 1", len(result.Tools))
	}
	if result.Tools[0].Name != "test" {
		t.Errorf("tool name = %v, want test", result.Tools[0].Name)
	}
}

func TestNewGuardian(t *testing.T) {
	g := NewGuardian("/usr/bin/echo", "hello")
	if g.cmd == nil {
		t.Fatal("cmd is nil")
	}
}

func TestStopIdempotent(t *testing.T) {
	g := NewGuardian("/usr/bin/echo")
	g.stopped = true

	// Should not panic
	if err := g.Stop(); err != nil {
		t.Errorf("stop: %v", err)
	}
}

func TestCallWhenStopped(t *testing.T) {
	g := NewGuardian("/usr/bin/echo")
	g.stopped = true

	_, err := g.call("test", nil)
	if err == nil {
		t.Error("expected error when stopped")
	}
}
