// tooltest-vertex — empirical verification harness for the Vertex AI
// Gemini function-calling round-trip.
//
// Three modes:
//
//   proper — follows ai.google.dev/gemini-api/docs/function-calling Step 4
//            verbatim: append response.Candidates[0].Content (which carries
//            the FunctionCall part) to history, then append the user's
//            FunctionResponse, then re-prompt for the final answer.
//
//   hack   — reproduces what shell-agent-v2 production currently does
//            in 1-round: the assistant turn is replayed as plain text
//            "[Calling: …]" (no FunctionCall part), followed by the
//            FunctionResponse.
//
//   loop   — multi-round repro for the prod symptom. We force-issue a
//            second user turn after the first tool result and replay
//            using placeholder substitution on every assistant turn.
//            If the model re-issues the same FunctionCall on round 2
//            instead of summarising, that's the loop the user reported.
//
// Tool: add_numbers(a, b int) -> int. Always returns 12 for the test
// query "what is 7+5?".
//
// Usage:
//   GOOGLE_CLOUD_PROJECT=foo go run ./cmd/tooltest-vertex {proper|hack|loop}
//
// Reads env GOOGLE_CLOUD_PROJECT (required), GOOGLE_CLOUD_LOCATION
// (default us-central1), TOOLTEST_MODEL (default gemini-2.5-flash).
//
// Authentication: ADC. Requires `gcloud auth application-default login`.
//
// Output: human-readable per-turn log. Final line:
//   PASS — model produced text answer, no re-issue
//   FAIL — model re-issued the function call
//   ERROR — protocol error / API error
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"google.golang.org/genai"
)

const (
	systemInstr = "You are a calculator. When the user asks for arithmetic, " +
		"call the add_numbers function. After the result is returned, " +
		"give the answer as plain text."
	userQuery = "what is 7+5?"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tooltest-vertex {proper|hack|loop}")
		os.Exit(2)
	}
	mode := os.Args[1]
	if mode != "proper" && mode != "hack" && mode != "loop" {
		fmt.Fprintf(os.Stderr, "unknown mode %q; want proper|hack|loop\n", mode)
		os.Exit(2)
	}

	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		fmt.Fprintln(os.Stderr, "GOOGLE_CLOUD_PROJECT is required")
		os.Exit(2)
	}
	location := os.Getenv("GOOGLE_CLOUD_LOCATION")
	if location == "" {
		location = "us-central1"
	}
	model := os.Getenv("TOOLTEST_MODEL")
	if model == "" {
		model = "gemini-2.5-flash"
	}

	ctx := context.Background()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  project,
		Location: location,
		Backend:  genai.BackendVertexAI,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: client: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("=== tooltest-vertex mode=%s model=%s ===\n", mode, model)

	tools := []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        "add_numbers",
			Description: "Add two integers and return the sum.",
			ParametersJsonSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "integer"},
					"b": map[string]any{"type": "integer"},
				},
				"required": []string{"a", "b"},
			},
		}},
	}}
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(systemInstr, genai.RoleUser),
		Tools:             tools,
	}

	if mode == "loop" {
		runLoop(ctx, client, model, cfg)
		return
	}

	history := []*genai.Content{
		genai.NewContentFromText(userQuery, genai.RoleUser),
	}

	// Turn 1: expect a FunctionCall.
	fmt.Println("\n--- Turn 1 (expecting FunctionCall) ---")
	dump("REQUEST", history)
	resp, err := client.Models.GenerateContent(ctx, model, history, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: turn 1: %v\n", err)
		os.Exit(1)
	}
	dumpResponse("RESPONSE", resp)

	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		fmt.Println("ERROR — empty response on turn 1")
		os.Exit(1)
	}

	// Find the FunctionCall part.
	var fc *genai.FunctionCall
	for _, p := range resp.Candidates[0].Content.Parts {
		if p.FunctionCall != nil {
			fc = p.FunctionCall
			break
		}
	}
	if fc == nil {
		fmt.Println("ERROR — turn 1 did not contain a FunctionCall part")
		os.Exit(1)
	}
	fmt.Printf("\nModel called: %s(%v)\n", fc.Name, fc.Args)

	// Append the assistant turn to history. This is where the modes diverge.
	switch mode {
	case "proper":
		// Per docs: append response.candidates[0].content verbatim.
		history = append(history, resp.Candidates[0].Content)
	case "hack":
		// Reproduce the production placeholder substitution: a plain-text
		// model message that says "[Calling: …]" and contains NO
		// FunctionCall part.
		placeholder := fmt.Sprintf("[Calling: %s]", fc.Name)
		history = append(history, genai.NewContentFromText(placeholder, genai.RoleModel))
	}

	// Append the FunctionResponse: 7 + 5 = 12.
	funcResp := map[string]any{"result": 12}
	history = append(history, &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			genai.NewPartFromFunctionResponse(fc.Name, funcResp),
		},
	})

	// Turn 2: expect a text final answer.
	fmt.Println("\n--- Turn 2 (expecting text answer mentioning 12) ---")
	dump("REQUEST", history)
	resp2, err := client.Models.GenerateContent(ctx, model, history, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: turn 2: %v\n", err)
		os.Exit(1)
	}
	dumpResponse("RESPONSE", resp2)

	if len(resp2.Candidates) == 0 || resp2.Candidates[0].Content == nil {
		fmt.Println("ERROR — empty response on turn 2")
		os.Exit(1)
	}

	// Decide pass/fail by inspecting the parts.
	var text2 string
	var fc2 *genai.FunctionCall
	for _, p := range resp2.Candidates[0].Content.Parts {
		if p.Text != "" {
			text2 += p.Text
		}
		if p.FunctionCall != nil && fc2 == nil {
			fc2 = p.FunctionCall
		}
	}
	fmt.Println()
	if fc2 != nil {
		fmt.Printf("FAIL — model re-issued FunctionCall %q (text part: %q)\n", fc2.Name, text2)
		os.Exit(1)
	}
	if text2 == "" {
		fmt.Println("FAIL — model produced no text answer (and no FunctionCall)")
		os.Exit(1)
	}
	fmt.Printf("PASS — model produced text answer: %q\n", text2)
}

// runLoop simulates the multi-round agentLoop using the production-style
// placeholder substitution on every assistant turn (no FunctionCall part
// echoed back). Counts how many times the model re-issues the same call
// before producing a text answer.
func runLoop(ctx context.Context, client *genai.Client, model string, cfg *genai.GenerateContentConfig) {
	const maxRounds = 6
	history := []*genai.Content{
		genai.NewContentFromText(userQuery, genai.RoleUser),
	}
	calls := 0
	for round := range maxRounds {
		fmt.Printf("\n--- Round %d ---\n", round)
		dump("REQUEST", history)
		resp, err := client.Models.GenerateContent(ctx, model, history, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: round %d: %v\n", round, err)
			os.Exit(1)
		}
		dumpResponse("RESPONSE", resp)

		if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
			fmt.Println("ERROR — empty response")
			os.Exit(1)
		}
		var fc *genai.FunctionCall
		var text string
		for _, p := range resp.Candidates[0].Content.Parts {
			if p.Text != "" {
				text += p.Text
			}
			if p.FunctionCall != nil && fc == nil {
				fc = p.FunctionCall
			}
		}
		if fc == nil {
			fmt.Printf("\nRound %d: model produced text answer (%q) — settled after %d call(s)\n", round, text, calls)
			if calls > 1 {
				fmt.Printf("FAIL — model re-issued the call %d times before settling\n", calls-1)
				os.Exit(1)
			}
			fmt.Println("PASS — single call, then settled")
			return
		}
		calls++
		fmt.Printf("\nRound %d: model called %s(%v) (text part: %q) — total calls so far: %d\n", round, fc.Name, fc.Args, text, calls)

		// Production-style placeholder save (NO FunctionCall echoed
		// back) — exactly what agent.go does today when resp.Content
		// is empty.
		placeholder := text
		if placeholder == "" {
			placeholder = fmt.Sprintf("[Calling: %s]", fc.Name)
		}
		history = append(history, genai.NewContentFromText(placeholder, genai.RoleModel))

		// Append the FunctionResponse (always 12 — same result every
		// round, mirroring the prod symptom of identical results).
		history = append(history, &genai.Content{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				genai.NewPartFromFunctionResponse(fc.Name, map[string]any{"result": 12}),
			},
		})
	}
	fmt.Printf("\nFAIL — model re-issued the call for %d rounds without settling (loop detected)\n", maxRounds)
	os.Exit(1)
}

func dump(label string, contents []*genai.Content) {
	fmt.Printf("[%s]\n", label)
	for i, c := range contents {
		fmt.Printf("  [%d] role=%s parts=%d\n", i, c.Role, len(c.Parts))
		for j, p := range c.Parts {
			switch {
			case p.Text != "":
				fmt.Printf("       part[%d] text: %q\n", j, p.Text)
			case p.FunctionCall != nil:
				args, _ := json.Marshal(p.FunctionCall.Args)
				fmt.Printf("       part[%d] FunctionCall: name=%s args=%s id=%q\n",
					j, p.FunctionCall.Name, args, p.FunctionCall.ID)
			case p.FunctionResponse != nil:
				resp, _ := json.Marshal(p.FunctionResponse.Response)
				fmt.Printf("       part[%d] FunctionResponse: name=%s resp=%s\n",
					j, p.FunctionResponse.Name, resp)
			default:
				fmt.Printf("       part[%d] other\n", j)
			}
		}
	}
}

func dumpResponse(label string, resp *genai.GenerateContentResponse) {
	if resp == nil || len(resp.Candidates) == 0 {
		fmt.Printf("[%s] (empty)\n", label)
		return
	}
	dump(label, []*genai.Content{resp.Candidates[0].Content})
}
