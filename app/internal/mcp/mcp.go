// Package mcp manages mcp-guardian stdio child process communication.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// StartTimeout is the deadline for guardian initialization.
const StartTimeout = 15 * time.Second

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ToolDef describes a tool exposed by an MCP server.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Guardian manages a single mcp-guardian stdio child process.
//
// Concurrency model:
//
//   - callMu serializes call() invocations so a write/read pair is atomic
//     for any one in-flight RPC. Held across stdout.Scan, which can block.
//   - stateMu guards the shutdown bits (stopped flag, stdin, cmd, tools, id).
//     It is intentionally NEVER held across blocking I/O so Stop can preempt
//     a blocked call() — Stop closes stdin and kills the process, which
//     unblocks the Scan in call() with a read error.
type Guardian struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Scanner
	callMu  sync.Mutex
	stateMu sync.Mutex
	id      int
	tools   []ToolDef
	stopped bool
}

// NewGuardian creates a guardian with the given binary path and arguments.
func NewGuardian(binaryPath string, args ...string) *Guardian {
	return &Guardian{
		cmd: exec.Command(binaryPath, args...),
	}
}

// Start spawns the guardian process, initializes the MCP session,
// and discovers available tools.
func (g *Guardian) Start() error {
	var err error
	g.stdin, err = g.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdoutPipe, err := g.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	g.stdout = bufio.NewScanner(stdoutPipe)
	g.stdout.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer

	if err := g.cmd.Start(); err != nil {
		return fmt.Errorf("start guardian: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		if err := g.initialize(); err != nil {
			done <- fmt.Errorf("initialize: %w", err)
			return
		}
		if err := g.refreshTools(); err != nil {
			done <- fmt.Errorf("refresh tools: %w", err)
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			g.Stop()
			return err
		}
		return nil
	case <-time.After(StartTimeout):
		g.Stop()
		return fmt.Errorf("guardian start timed out after %s", StartTimeout)
	}
}

// Stop terminates the guardian process. Safe to call from any goroutine
// concurrently with an in-flight call(): closing stdin and killing the
// process unblocks the call's stdout.Scan with a read error.
func (g *Guardian) Stop() error {
	g.stateMu.Lock()
	if g.stopped {
		g.stateMu.Unlock()
		return nil
	}
	g.stopped = true
	stdin := g.stdin
	g.stdin = nil
	cmd := g.cmd
	g.stateMu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// Tools returns the discovered MCP tools.
func (g *Guardian) Tools() []ToolDef {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	return g.tools
}

// CallTool invokes an MCP tool and returns the result.
func (g *Guardian) CallTool(name string, arguments json.RawMessage) (json.RawMessage, error) {
	resp, err := g.call("tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return nil, err
	}
	return resp.Result, nil
}

// --- internal ---

func (g *Guardian) initialize() error {
	_, err := g.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "shell-agent-v2",
			"version": "0.1.0",
		},
	})
	return err
}

func (g *Guardian) refreshTools() error {
	resp, err := g.call("tools/list", nil)
	if err != nil {
		return err
	}

	var result struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("parse tools: %w", err)
	}
	g.stateMu.Lock()
	g.tools = result.Tools
	g.stateMu.Unlock()
	return nil
}

// call performs a JSON-RPC round trip. The blocking stdout.Scan runs
// outside stateMu so Stop can interrupt it by closing stdin and killing
// the process.
func (g *Guardian) call(method string, params any) (*Response, error) {
	g.callMu.Lock()
	defer g.callMu.Unlock()

	g.stateMu.Lock()
	if g.stopped || g.stdin == nil {
		g.stateMu.Unlock()
		return nil, fmt.Errorf("guardian is stopped")
	}
	g.id++
	id := g.id
	stdin := g.stdin
	g.stateMu.Unlock()

	req := Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	if _, err := fmt.Fprintf(stdin, "%s\n", data); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	if !g.stdout.Scan() {
		if err := g.stdout.Err(); err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		return nil, fmt.Errorf("read response: EOF")
	}

	var resp Response
	if err := json.Unmarshal(g.stdout.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("RPC error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return &resp, nil
}
