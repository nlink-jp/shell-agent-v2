// llm-cache-bench probes a local OpenAI-compatible endpoint (e.g.
// LM Studio) to empirically check whether prompt-prefix KV cache
// reuse fires under various message-construction strategies. Used
// to validate the design hypotheses behind ADR-0017 (prompt
// prefix stability for KV cache reuse) before we commit to the
// implementation.
//
// Usage:
//
//	go run ./cmd/llm-cache-bench \
//	    --endpoint http://localhost:1234/v1 \
//	    --model google/gemma-4-26b-a4b \
//	    --runs 5
//
// The probe is read-only on the LLM side (no tools, no streaming);
// it only measures the wall-clock time for short `max_tokens=5`
// completions across scenarios that vary the prefix-stability of
// the request. Output is a markdown report on stdout.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// --- Wire types ----------------------------------------------------

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Stream      bool          `json:"stream"`
	Temperature float64       `json:"temperature"`
	// cache_prompt is a llama.cpp / llama-server extension that
	// some OpenAI-compat servers ignore, some accept, and some
	// reject. T5 probes which it is.
	CachePrompt *bool `json:"cache_prompt,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		CachedTokens     int `json:"cached_tokens,omitempty"`
	} `json:"usage"`
	// LM Studio may include extra timing fields here. We capture
	// the raw response so the markdown report can surface them.
	Raw json.RawMessage `json:"-"`
}

// --- HTTP client ---------------------------------------------------

type client struct {
	endpoint string
	model    string
	http     *http.Client
}

// callResult bundles the per-request metrics. Errors are surfaced
// without aborting the suite so we can record which scenarios fail
// (e.g. cache_prompt being rejected).
type callResult struct {
	WallMs       int64
	PromptTokens int
	OutTokens    int
	CachedTokens int
	Err          error
	HTTPStatus   int
	RawSnippet   string // first ~200 chars of the response body on error
}

func (c *client) call(messages []chatMessage, cachePrompt *bool) callResult {
	body := chatRequest{
		Model:       c.model,
		Messages:    messages,
		MaxTokens:   5,
		Stream:      false,
		Temperature: 0,
		CachePrompt: cachePrompt,
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", c.endpoint+"/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return callResult{Err: err}
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	wallMs := time.Since(start).Milliseconds()

	if resp.StatusCode != 200 {
		snip := string(rawBody)
		if len(snip) > 200 {
			snip = snip[:200]
		}
		return callResult{
			WallMs:     wallMs,
			HTTPStatus: resp.StatusCode,
			RawSnippet: snip,
			Err:        fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}

	var cr chatResponse
	if err := json.Unmarshal(rawBody, &cr); err != nil {
		return callResult{WallMs: wallMs, HTTPStatus: resp.StatusCode, Err: err}
	}
	return callResult{
		WallMs:       wallMs,
		PromptTokens: cr.Usage.PromptTokens,
		OutTokens:    cr.Usage.CompletionTokens,
		CachedTokens: cr.Usage.CachedTokens,
		HTTPStatus:   resp.StatusCode,
	}
}

// --- Scenarios -----------------------------------------------------

// scenario captures the setup for one experiment plus the runs that
// were actually executed (separately tracked so a scenario can fail
// halfway through without crashing the suite).
type scenario struct {
	ID    string
	Title string
	Notes string
	Runs  []callResult
}

func (s *scenario) firstRunMs() int64 {
	if len(s.Runs) == 0 {
		return 0
	}
	return s.Runs[0].WallMs
}

// meanSubsequentMs averages runs 2..N (excludes the first, which
// is always cold under any caching strategy).
func (s *scenario) meanSubsequentMs() int64 {
	if len(s.Runs) < 2 {
		return 0
	}
	var sum int64
	var n int
	for _, r := range s.Runs[1:] {
		if r.Err != nil {
			continue
		}
		sum += r.WallMs
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / int64(n)
}

// speedupPct returns the (first - mean_subsequent) / first ratio
// expressed as a percentage. Big positive number = cache works.
func (s *scenario) speedupPct() float64 {
	first := s.firstRunMs()
	subseq := s.meanSubsequentMs()
	if first == 0 {
		return 0
	}
	return float64(first-subseq) / float64(first) * 100
}

// makeBigPad returns a deterministic string of approximately N
// tokens worth of filler text. The text is paragraph-like English
// so the tokenizer treats it normally (and so it doesn't trigger
// safety filters). Used for T6 to make the prompt long enough for
// caching effects to be measurable.
func makeBigPad(targetTokens int) string {
	// Crude approximation: 1 token ≈ 4 chars of English prose.
	targetChars := targetTokens * 4
	var b strings.Builder
	b.Grow(targetChars)
	chunk := "The quick brown fox jumps over the lazy dog while reading a long technical document about distributed systems and consensus protocols. "
	for b.Len() < targetChars {
		b.WriteString(chunk)
	}
	return b.String()[:targetChars]
}

func runT1SameRequest(c *client, runs int) scenario {
	s := scenario{
		ID:    "T1",
		Title: "Same system + same user, repeated",
		Notes: "If the server caches the KV across requests, the second and following calls should be substantially faster than the first.",
	}
	system := "You are a helpful assistant. Answer briefly."
	user := "Reply with the word OK."
	for i := 0; i < runs; i++ {
		s.Runs = append(s.Runs, c.call([]chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		}, nil))
	}
	return s
}

func runT2StableSystemVaryingUserTail(c *client, runs int) scenario {
	s := scenario{
		ID:    "T2",
		Title: "Same system, user message varies only in trailing token",
		Notes: "Probes whether prefix match extends past the system prompt into the user message. Each user prompt shares a long prefix and differs only at the end.",
	}
	system := "You are a helpful assistant. Answer briefly."
	for i := 0; i < runs; i++ {
		user := fmt.Sprintf("Reply with the word OK. Iteration index: %d.", i)
		s.Runs = append(s.Runs, c.call([]chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		}, nil))
	}
	return s
}

func runT3VolatileSystemTimestamp(c *client, runs int) scenario {
	s := scenario{
		ID:    "T3",
		Title: "Volatile system prompt (timestamp inside system), stable user — simulates current shell-agent-v2",
		Notes: "Reproduces the current production layout where temporal context is embedded in the system prompt. Expectation: little to no cache benefit because the system prefix changes every call.",
	}
	user := "Reply with the word OK."
	for i := 0; i < runs; i++ {
		ts := time.Now().Format("2006-01-02 15:04:05.000")
		system := "You are a helpful assistant.\n\nCurrent date and time: " + ts + "\n\nAnswer briefly."
		s.Runs = append(s.Runs, c.call([]chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		}, nil))
		// Sleep a bit so the timestamp actually changes between
		// calls — otherwise on a fast loop two requests can land
		// in the same millisecond.
		time.Sleep(50 * time.Millisecond)
	}
	return s
}

func runT4StableSystemUserSideTimestamp(c *client, runs int) scenario {
	s := scenario{
		ID:    "T4",
		Title: "Stable system, timestamp moved to user message — simulates the ADR-0017 proposed layout",
		Notes: "System prompt is byte-identical across calls; the timestamp lives at the head of the user message. Expectation: cache should fire for the system block, leaving only the user message to reprocess.",
	}
	system := "You are a helpful assistant. Answer briefly."
	for i := 0; i < runs; i++ {
		ts := time.Now().Format("2006-01-02 15:04:05.000")
		user := "Current date and time: " + ts + "\n\nReply with the word OK."
		s.Runs = append(s.Runs, c.call([]chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		}, nil))
		time.Sleep(50 * time.Millisecond)
	}
	return s
}

func runT5CachePromptParameter(c *client, runs int) scenario {
	s := scenario{
		ID:    "T5",
		Title: "Probe `cache_prompt: true` extension parameter",
		Notes: "Sends the same baseline as T1 but with `cache_prompt: true` in the request body. Determines whether the server accepts (ignores or honours) the parameter or rejects the whole request with 4xx.",
	}
	system := "You are a helpful assistant. Answer briefly."
	user := "Reply with the word OK."
	yes := true
	for i := 0; i < runs; i++ {
		s.Runs = append(s.Runs, c.call([]chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		}, &yes))
	}
	return s
}

func runT7LargeVolatileSystem(c *client, runs int) scenario {
	s := scenario{
		ID:    "T7",
		Title: "Large system + volatile timestamp INSIDE system (current shell-agent-v2 layout, full size)",
		Notes: "Repeats T3 but with 8K filler so prompt processing is non-trivial. Expectation: NO cache reuse because the system prefix differs every call — each iteration pays the full ~6 s prompt-processing cost.",
	}
	pad := makeBigPad(8000)
	user := "Reply with the word OK."
	for i := 0; i < runs; i++ {
		ts := time.Now().Format("2006-01-02 15:04:05.000")
		system := "You are a helpful assistant.\n\nCurrent date and time: " + ts + "\n\nReference document:\n\n" + pad + "\n\nAnswer briefly."
		s.Runs = append(s.Runs, c.call([]chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		}, nil))
		time.Sleep(50 * time.Millisecond)
	}
	return s
}

func runT8LargeStableSystemUserTimestamp(c *client, runs int) scenario {
	s := scenario{
		ID:    "T8",
		Title: "Large system stable + timestamp in user message (ADR-0017 proposed layout, full size)",
		Notes: "Repeats T4 with 8K filler. Expectation: cache fires on the system block, subsequent calls drop to ~100 ms — matching the T6 result and demonstrating the design works in realistic conditions.",
	}
	pad := makeBigPad(8000)
	system := "You are a helpful assistant.\n\nReference document:\n\n" + pad + "\n\nAnswer briefly."
	for i := 0; i < runs; i++ {
		ts := time.Now().Format("2006-01-02 15:04:05.000")
		user := "Current date and time: " + ts + "\n\nReply with the word OK."
		s.Runs = append(s.Runs, c.call([]chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		}, nil))
		time.Sleep(50 * time.Millisecond)
	}
	return s
}

// runT9MemoryVolatility measures the cost of changing one memory
// section mid-conversation. Memory blocks in shell-agent-v2 sit at
// the END of the system prompt today (Global Memory → Session
// Memory → Findings). Phase 2 of ADR-0017 needs to know whether
// adding a single fact between turns is cheap (server reuses most
// of the cache, only the added line costs) or expensive (memory
// invalidation cascades through the rest of the prompt).
//
// Pattern (5-run minimum; later runs repeat the last memory):
//
//	run 1: memory_v1  — cold call, establishes baseline prompt cost
//	run 2: memory_v1  — should cache hit (warm baseline)
//	run 3: memory_v2  — memory grew by 1 fact (~10 tokens); measures penalty
//	run 4: memory_v2  — should cache hit for v2 (warm v2 baseline)
//	run 5: memory_v3  — memory grew by another fact; confirms pattern
//
// If wall_ms(3) ≈ wall_ms(2), memory volatility is essentially free.
// If wall_ms(3) ≈ wall_ms(1), each memory mutation costs a full
// reprocess. Real answer expected in between.
func runT9MemoryVolatility(c *client, runs int) scenario {
	s := scenario{
		ID:    "T9",
		Title: "Memory section grows by one fact between turns (Phase 2 question)",
		Notes: "Run 1-2 share memory v1; run 3-4 share memory v2 (1 new fact); run 5 has v3 (1 more new fact). Compare run 2 (cache hit on stable memory) vs run 3 (penalty when memory grows). Same large system prompt + same short history throughout.",
	}
	pad := makeBigPad(7000)
	baseSystem := "You are a helpful assistant.\n\nReference document:\n\n" + pad
	memoryA := "\n\nImportant facts:\n- The user's name is Alice.\n- They prefer concise answers."
	memoryB := memoryA + "\n- They live in Tokyo."
	memoryC := memoryB + "\n- They use Python for data analysis."
	history := []chatMessage{
		{Role: "user", Content: "Hi"},
		{Role: "assistant", Content: "Hello"},
		{Role: "user", Content: "How are you?"},
		{Role: "assistant", Content: "Doing well, thank you."},
	}
	user := "Reply with the word OK."

	// Memory schedule keyed to the first 5 runs. Beyond run 5 we
	// stay at v3 so any extra --runs just gives more warm samples
	// on v3.
	memSchedule := []string{memoryA, memoryA, memoryB, memoryB, memoryC}
	for i := 0; i < runs; i++ {
		mem := memSchedule[len(memSchedule)-1]
		if i < len(memSchedule) {
			mem = memSchedule[i]
		}
		system := baseSystem + mem
		msgs := []chatMessage{{Role: "system", Content: system}}
		msgs = append(msgs, history...)
		msgs = append(msgs, chatMessage{Role: "user", Content: user})
		s.Runs = append(s.Runs, c.call(msgs, nil))
		time.Sleep(50 * time.Millisecond)
	}
	return s
}

func runT6LargeStablePrefix(c *client, runs int) scenario {
	s := scenario{
		ID:    "T6",
		Title: "Large stable system prompt (~8K tokens of filler)",
		Notes: "Worst-case prompt-processing scenario: a long stable system prompt. Cache benefit should be most visible here in absolute terms (large prompt_processing baseline shrinks to ~0 on cache hit).",
	}
	system := "You are a helpful assistant.\n\nReference document:\n\n" + makeBigPad(8000) + "\n\nAnswer briefly using only the document above."
	user := "Reply with the word OK."
	for i := 0; i < runs; i++ {
		s.Runs = append(s.Runs, c.call([]chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		}, nil))
	}
	return s
}

// --- Output --------------------------------------------------------

func writeMarkdown(w io.Writer, endpoint, model string, runs int, scenarios []scenario) {
	fmt.Fprintf(w, "# LM Studio Prompt Cache Benchmark\n\n")
	fmt.Fprintf(w, "- **Endpoint**: `%s`\n", endpoint)
	fmt.Fprintf(w, "- **Model**: `%s`\n", model)
	fmt.Fprintf(w, "- **Runs per scenario**: %d (first call always cold)\n", runs)
	fmt.Fprintf(w, "- **Date**: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	fmt.Fprintf(w, "All requests use `max_tokens: 5`, `temperature: 0`, `stream: false`. Wall-clock ms ≈ prompt-processing time + ~5-token generation overhead (negligible). A large gap between the first call and subsequent calls implies the server is reusing cached prompt KV.\n\n")

	for _, s := range scenarios {
		fmt.Fprintf(w, "## %s — %s\n\n", s.ID, s.Title)
		if s.Notes != "" {
			fmt.Fprintf(w, "%s\n\n", s.Notes)
		}
		fmt.Fprintf(w, "| Run | wall ms | prompt tok | out tok | cached tok | http | error |\n")
		fmt.Fprintf(w, "|---:|---:|---:|---:|---:|---:|---|\n")
		for i, r := range s.Runs {
			errStr := ""
			if r.Err != nil {
				errStr = r.Err.Error()
				if r.RawSnippet != "" {
					errStr += " — " + strings.ReplaceAll(r.RawSnippet, "\n", " ")
				}
			}
			fmt.Fprintf(w, "| %d | %d | %d | %d | %d | %d | %s |\n",
				i+1, r.WallMs, r.PromptTokens, r.OutTokens, r.CachedTokens, r.HTTPStatus, errStr)
		}
		fmt.Fprintf(w, "\n")
		if len(s.Runs) >= 2 {
			fmt.Fprintf(w, "**Summary**: first=%dms, mean(subsequent)=%dms, speedup=%.1f%%\n\n",
				s.firstRunMs(), s.meanSubsequentMs(), s.speedupPct())
		}
	}

	// Aggregate
	fmt.Fprintf(w, "## Cross-scenario summary\n\n")
	fmt.Fprintf(w, "| Scenario | First (ms) | Subseq mean (ms) | Speedup | Cache observed? |\n")
	fmt.Fprintf(w, "|---|---:|---:|---:|---|\n")
	for _, s := range scenarios {
		hit := "—"
		if len(s.Runs) >= 2 && s.Runs[0].Err == nil {
			if s.speedupPct() >= 30 {
				hit = "✅ yes (≥30%)"
			} else if s.speedupPct() >= 10 {
				hit = "~ marginal"
			} else {
				hit = "❌ no"
			}
		}
		first := s.firstRunMs()
		subseq := s.meanSubsequentMs()
		fmt.Fprintf(w, "| %s | %d | %d | %.1f%% | %s |\n", s.ID, first, subseq, s.speedupPct(), hit)
	}
	fmt.Fprintln(w)
}

// --- Entrypoint ----------------------------------------------------

func main() {
	endpoint := flag.String("endpoint", "http://localhost:1234/v1", "OpenAI-compatible API base URL (no trailing slash)")
	model := flag.String("model", "google/gemma-4-26b-a4b", "Model identifier as registered with the server")
	runs := flag.Int("runs", 5, "Number of calls per scenario (first is the cold-call reference)")
	outPath := flag.String("out", "", "Optional path to also write the markdown report. Always prints to stdout.")
	flag.Parse()

	c := &client{
		endpoint: strings.TrimRight(*endpoint, "/"),
		model:    *model,
		http: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}

	// Warm-up: 2 calls with a unique prompt so the model is loaded
	// and any per-process JIT compilation is done before we begin
	// measuring. The first scenario's "first call" still represents
	// the cold-prefix case for THAT scenario's prompt — we're not
	// trying to remove that signal; we just don't want the first
	// scenario to also catch model-load latency.
	fmt.Fprintln(os.Stderr, "warm-up...")
	for i := 0; i < 2; i++ {
		_ = c.call([]chatMessage{
			{Role: "system", Content: "warm-up"},
			{Role: "user", Content: fmt.Sprintf("warm-up iteration %d", i)},
		}, nil)
	}

	fmt.Fprintln(os.Stderr, "running scenarios...")
	scenarios := []scenario{
		runT1SameRequest(c, *runs),
		runT2StableSystemVaryingUserTail(c, *runs),
		runT3VolatileSystemTimestamp(c, *runs),
		runT4StableSystemUserSideTimestamp(c, *runs),
		runT5CachePromptParameter(c, *runs),
		runT6LargeStablePrefix(c, *runs),
		runT7LargeVolatileSystem(c, *runs),
		runT8LargeStableSystemUserTimestamp(c, *runs),
		runT9MemoryVolatility(c, *runs),
	}

	writeMarkdown(os.Stdout, c.endpoint, c.model, *runs, scenarios)

	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: could not write report file: %v\n", err)
			return
		}
		defer f.Close()
		writeMarkdown(f, c.endpoint, c.model, *runs, scenarios)
		fmt.Fprintf(os.Stderr, "report saved to %s\n", *outPath)
	}
}
