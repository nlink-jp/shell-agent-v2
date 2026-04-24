package toolcall

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const testScript = `#!/bin/bash
# @tool: test-echo
# @description: Echo back the input
# @param: message string "Message to echo"
# @category: read

cat
`

const testWriteScript = `#!/bin/bash
# @tool: test-write
# @description: Write something
# @param: path string "File path"
# @param: content string "Content to write"
# @category: write

echo "would write"
`

func TestParseToolHeader(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "test-echo.sh")
	os.WriteFile(scriptPath, []byte(testScript), 0755)

	tool, err := parseToolHeader(scriptPath)
	if err != nil {
		t.Fatalf("parseToolHeader: %v", err)
	}
	if tool == nil {
		t.Fatal("tool is nil")
	}
	if tool.Name != "test-echo" {
		t.Errorf("name = %v, want test-echo", tool.Name)
	}
	if tool.Description != "Echo back the input" {
		t.Errorf("description = %v", tool.Description)
	}
	if tool.Category != CategoryRead {
		t.Errorf("category = %v, want read", tool.Category)
	}
	if len(tool.Params) != 1 {
		t.Fatalf("params count = %d, want 1", len(tool.Params))
	}
	if tool.Params[0].Name != "message" {
		t.Errorf("param name = %v, want message", tool.Params[0].Name)
	}
}

func TestNeedsMITL(t *testing.T) {
	read := &Tool{Category: CategoryRead}
	write := &Tool{Category: CategoryWrite}
	execute := &Tool{Category: CategoryExecute}

	if read.NeedsMITL() {
		t.Error("read should not need MITL")
	}
	if !write.NeedsMITL() {
		t.Error("write should need MITL")
	}
	if !execute.NeedsMITL() {
		t.Error("execute should need MITL")
	}
}

func TestScanDir(t *testing.T) {
	tmpDir := t.TempDir()

	os.WriteFile(filepath.Join(tmpDir, "echo.sh"), []byte(testScript), 0755)
	os.WriteFile(filepath.Join(tmpDir, "write.sh"), []byte(testWriteScript), 0755)
	os.WriteFile(filepath.Join(tmpDir, "plain.txt"), []byte("not a tool"), 0644)

	r := NewRegistry()
	if err := r.ScanDir(tmpDir); err != nil {
		t.Fatalf("ScanDir: %v", err)
	}

	all := r.All()
	if len(all) != 2 {
		t.Errorf("tools count = %d, want 2", len(all))
	}

	tool, ok := r.Get("test-echo")
	if !ok {
		t.Fatal("test-echo not found")
	}
	if tool.Category != CategoryRead {
		t.Errorf("test-echo category = %v", tool.Category)
	}

	tool, ok = r.Get("test-write")
	if !ok {
		t.Fatal("test-write not found")
	}
	if tool.Category != CategoryWrite {
		t.Errorf("test-write category = %v", tool.Category)
	}
}

func TestScanDirNonExistent(t *testing.T) {
	r := NewRegistry()
	err := r.ScanDir("/nonexistent/path")
	if err != nil {
		t.Errorf("non-existent dir should not error: %v", err)
	}
}

func TestExecute(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "echo.sh")
	os.WriteFile(scriptPath, []byte(testScript), 0755)

	tool := &Tool{
		Name:       "test-echo",
		ScriptPath: scriptPath,
		Category:   CategoryRead,
	}

	result, err := Execute(context.Background(), tool, `{"message":"hello"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != `{"message":"hello"}` {
		t.Errorf("result = %q, want JSON input echoed back", result)
	}
}

func TestToolDefParams(t *testing.T) {
	tool := &Tool{
		Params: []Param{
			{Name: "path", Type: "string", Description: "File path"},
			{Name: "force", Type: "boolean", Description: "Force overwrite"},
		},
	}

	def := tool.ToolDefParams()
	if def["type"] != "object" {
		t.Error("type should be object")
	}
	props, ok := def["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be map")
	}
	if len(props) != 2 {
		t.Errorf("properties count = %d, want 2", len(props))
	}
}
