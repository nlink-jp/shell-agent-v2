// Package llm — request-dump diagnostic.
//
// When the environment variable SHELL_AGENT_DEBUG_LLM is set
// (any non-empty value), every outgoing chat-completion request
// body is appended to ~/Library/Application Support/shell-agent-v2/
// llm-debug.log as a one-line JSON record, along with a sha256
// fingerprint and a stable-prefix fingerprint of the first 4 KiB.
//
// The point is byte-level diagnosis of why KV-cache prefix reuse
// is (or isn't) firing. Two consecutive turns of the same session
// should share the longest possible byte prefix; comparing the
// dumped bodies makes any unexpected per-turn volatility obvious
// (e.g. a still-rotating nonce, a timestamp we forgot to lift, a
// memory section that grew).
//
// Off by default. The file is plain JSONL, append-only, no
// rotation — set the env var, reproduce, inspect, unset.
package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/genai"
)

// dumpEnabled caches the env-var check so we don't os.Getenv per
// request. SHELL_AGENT_DEBUG_LLM=1 (or any non-empty) → on.
var dumpEnabled = func() bool {
	return os.Getenv("SHELL_AGENT_DEBUG_LLM") != ""
}()

// dumpMu serialises writes so concurrent agent loop rounds don't
// interleave their JSONL records.
var dumpMu sync.Mutex

// dumpRequestBody writes one JSONL record to llm-debug.log when
// the diagnostic is enabled. The record includes:
//
//   - timestamp: ISO-8601 UTC
//   - backend: "local" / "vertex_ai"
//   - body_sha256: full-body fingerprint (changes ↔ anything in
//     the request body changed)
//   - prefix_sha256: fingerprint of the first 4 KiB of the body
//     (changes ↔ the system block / start of history changed —
//     useful to diagnose "the cache miss is right at the start")
//   - bytes: full body length in bytes
//   - body: the raw request body (UTF-8 string)
//
// All failures are swallowed silently — this is debug
// instrumentation, not load-bearing.
func dumpRequestBody(backend string, body []byte) {
	if !dumpEnabled {
		return
	}
	path := dumpPath()
	if path == "" {
		return
	}

	full := sha256.Sum256(body)
	prefixLen := 4096
	if prefixLen > len(body) {
		prefixLen = len(body)
	}
	prefix := sha256.Sum256(body[:prefixLen])

	record := struct {
		Timestamp string `json:"timestamp"`
		Backend   string `json:"backend"`
		BodySha   string `json:"body_sha256"`
		PrefixSha string `json:"prefix_sha256"`
		Bytes     int    `json:"bytes"`
		Body      string `json:"body"`
	}{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Backend:   backend,
		BodySha:   hex.EncodeToString(full[:]),
		PrefixSha: hex.EncodeToString(prefix[:]),
		Bytes:     len(body),
		Body:      string(body),
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}

	dumpMu.Lock()
	defer dumpMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(line)
	_, _ = f.Write([]byte("\n"))
}

// dumpPath returns the JSONL output path, honouring HOME so tests
// (and the diagnostic env-var workflow) write to a predictable
// place. Returns "" if HOME isn't resolvable — the dump becomes a
// no-op rather than crashing the request.
func dumpPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "shell-agent-v2", "llm-debug.log")
}

// dumpVertexRequest is the Vertex AI analogue of dumpRequestBody.
// The genai SDK constructs the wire request internally, so we
// serialise (gcConfig + contents) instead — that's the actual input
// to the request and what matters for prefix-stability analysis.
// Format is the same JSONL schema as dumpRequestBody, with the
// body field replaced by a structured {system_instruction,
// contents} pair.
func dumpVertexRequest(cfg *genai.GenerateContentConfig, contents []*genai.Content) {
	if !dumpEnabled {
		return
	}
	path := dumpPath()
	if path == "" {
		return
	}

	payload := struct {
		System   any `json:"system_instruction"`
		Contents any `json:"contents"`
	}{
		System:   cfg.SystemInstruction,
		Contents: contents,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	full := sha256.Sum256(body)
	prefixLen := 4096
	if prefixLen > len(body) {
		prefixLen = len(body)
	}
	prefix := sha256.Sum256(body[:prefixLen])

	record := struct {
		Timestamp string `json:"timestamp"`
		Backend   string `json:"backend"`
		BodySha   string `json:"body_sha256"`
		PrefixSha string `json:"prefix_sha256"`
		Bytes     int    `json:"bytes"`
		Body      string `json:"body"`
	}{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Backend:   "vertex_ai",
		BodySha:   hex.EncodeToString(full[:]),
		PrefixSha: hex.EncodeToString(prefix[:]),
		Bytes:     len(body),
		Body:      string(body),
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}

	dumpMu.Lock()
	defer dumpMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(line)
	_, _ = f.Write([]byte("\n"))
}
