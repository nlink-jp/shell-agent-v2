# System Rules — user-defined agent instructions — Design Note

**Status:** Design draft (2026-05-15); pending approval.
**Target:** v0.7.0 (minor bump on top of v0.6.6 — new user-facing
subsystem with config schema extension, no breaking changes).
**Reported by:** User — wants a way to set durable, declarative
agent instructions ("the agent should always do X, never do Y")
that are separate from Global Memory and survive across all
sessions, in the spirit of `AGENTS.md` / `CLAUDE.md` style
project rules.

This note specifies a new **System Rules** facility: a single
free-form Markdown file the user authors, injected into the
system prompt at every turn near the top, edited via a Settings
panel (and optionally in any external editor).

---

## 1. Problem

shell-agent-v2 already has four memory facilities (Records,
Session Memory, Findings, Global Memory; see ADR / memory-model
docs). All four are *learned at runtime*:

- Session Memory captures fact/context the agent extracted during
  a conversation.
- Global Memory pins preference/decision-shaped items the user
  explicitly promoted.
- Findings captures data-analysis discoveries.
- Records is the conversation transcript.

None of these accept **declarative, intentional, durable
instructions** that the user authors up-front to shape *how* the
agent behaves across every session — the equivalent of
`AGENTS.md` / `CLAUDE.md` in the Claude Code world. Workarounds
today are awkward:

1. Pin a long instruction text into Global Memory: but the Global
   Memory category vocabulary is `preference` / `decision` and
   the model is "things learned about the user", not "instructions
   the user gave". Mixing these confuses promotion logic and the
   UI presentation.
2. Edit the base system prompt: but that lives in code
   (`internal/chat/chat.go` `defaultSystemPrompt`) and requires
   a rebuild.
3. Repeat the instructions every session in the first user
   message: tedious and easy to forget.

There is no facility for *"the user has said, once and durably,
that the agent should always behave this way"*.

---

## 2. Goals

1. **One global Markdown file** at `<dataDir>/system_rules.md`
   that the user authors. Content is free-form; no schema, no
   parsing.
2. **Edited via Settings UI** — new "System Rules" section with a
   textarea, character and estimated-token counter, Save button.
   File path also displayed so external-editor workflow works.
3. **Injected near the top of the system prompt**, between the
   base prompt and temporal context, with a clear marker block.
   Empty file → no injection (no header, no marker).
4. **Hot reload on Save** — no app restart required. New rules
   apply to the next user turn.
5. **Token-budget aware** — the Settings UI shows estimated tokens
   and a soft warning when the file would consume a meaningful
   fraction of the active backend's context budget.
6. **Survives config migrations** — System Rules content lives in
   its own file, not inside `config.json`, so a future config
   schema change cannot corrupt or drop it.

Non-goals:

- **Per-session overrides.** A future "session profile" facility
  could layer on top, but the v0.7.0 scope is global-only.
- **Multiple rule files / per-project rules.** shell-agent-v2 is
  a GUI app with no project-directory concept; the Claude Code
  CLAUDE.md-discovery pattern does not map cleanly.
- **Rule templating / variable expansion.** Plain Markdown only.
  No `{{date}}` / `{{user}}` substitution. The base system prompt
  already injects temporal context separately.
- **Versioning / history of past rule content.** The user owns
  the file; if they want version control they can keep their own
  copy in git.
- **Sync between machines.** Out of scope for a local-first tool.

---

## 3. Design

### 3.1 Storage

- File: `<dataDir>/system_rules.md`
  (`~/Library/Application Support/shell-agent-v2/system_rules.md`
  on macOS).
- Format: UTF-8 Markdown, no frontmatter, no schema. Trailing
  newline normalised on Save.
- Encoding-safety: file is treated as opaque text in the agent
  pipeline; only normalisation applied is `strings.ReplaceAll(s,
  "\r\n", "\n")` and a `strings.TrimRight(s, "\n") + "\n"` to
  pin one trailing newline.
- Missing file: equivalent to empty content. No error surfaced.
- Atomic write: via `internal/atomicio.WriteFileAtomic` (tmp +
  rename + parent-dir fsync), same as every other JSON state
  file. Mode 0600.

### 3.2 New package `internal/sysrules/`

Small, single-responsibility:

```go
package sysrules

type Store struct {
    path    string
    content string       // in-memory cache
}

func NewStore() *Store                        // path = config.SystemRulesPath()
func NewStoreAt(path string) *Store           // test helper
func (s *Store) Path() string
func (s *Store) Load() error                  // read from disk into cache; missing file → empty
func (s *Store) Save(content string) error    // normalise + atomic write + cache update
func (s *Store) Get() string                  // snapshot from cache
```

`Load` is called once at app start. `Save` is called by the
binding when the user clicks Save in Settings. The Store has
**no internal mutex** — all access flows through `Agent` which
serialises with its own `a.mu`, matching the existing
`internal/memory/global_memory.go` `GlobalMemoryStore` pattern.

Token estimation lives in `internal/memory/tokens.go`
(`EstimateTokens(s string) int`) and is re-used directly by the
frontend via a local approximation. No backend RPC per keystroke;
see §3.6.

### 3.3 Injection into `chat.Engine.BuildSystemPrompt`

The existing three memory channels (`globalMemoryContext`,
`sessionMemoryContext`, `findingsContext`) are passed to
`BuildSystemPrompt` as **parameters**, not held on the `Engine`
struct. System Rules follow the same pattern: add a fourth
parameter rather than introducing a new mutable engine field.

This avoids a data race between the turn goroutine (which calls
`BuildSystemPrompt` without holding `a.mu`) and the Settings
goroutine (which would write to an engine field under `a.mu`).
The `Engine` stays a near-pure-function module over its inputs;
the agent layer owns all mutable state.

```go
func (e *Engine) BuildSystemPrompt(
    globalMemoryContext, sessionMemoryContext, findingsContext, systemRules string,
) string {
    e.guardTag = guard.NewTag()
    timeContext := buildTemporalContext()
    if e.location != "" {
        timeContext += "\nLocation: " + e.location
    }
    full := e.systemPrompt
    systemRules = strings.TrimSpace(systemRules)
    if systemRules != "" {
        full += "\n\n" +
            "The user has defined the following standing instructions. " +
            "Treat them as high-priority rules that override the default agent behaviour unless they conflict with safety or security guidelines.\n\n" +
            "<system_rules>\n" + systemRules + "\n</system_rules>"
    }
    full += "\n\n" + timeContext
    if e.sandboxEnabled {
        full += sandboxGuidance
    }
    // ... existing global / session / findings injection unchanged
    return full
}
```

Marker rationale (`<system_rules>` / `</system_rules>` envelope):

- The LLM can clearly identify the trust boundary (user-authored
  high-priority instructions, distinct from data).
- The rendering is stable for debugging / log review.
- The tag is fixed (not nonce-rotated like the user-data guard).
  This is **intentional**: system rules are trusted content from
  the user, not untrusted data that needs prompt-injection
  defense. The reverse trust direction.

Position rationale (recap):

- After **base prompt**: the base prompt sets the agent's core
  identity, operational role, and tool-use protocol. System rules
  are user-defined *adjustments* to that role; they make sense
  after the role is established but before any contextual data.
- Before **temporal context**: temporal context is data ("today
  is …"), not instruction. Rules belong with instructions.
- Before **sandbox guidance** and **memory blocks**: these are
  derived from runtime state. Rules are stable.

This matches the prompt-injection-defense memory entry
`feedback_prompt_injection_position` (defense instructions at
the top), generalised: *all* trusted instruction surface goes
before *any* data surface.

### 3.4 Bindings

Two new methods on the Wails binding layer (`app/bindings.go`,
thin delegation):

```go
func (b *Bindings) GetSystemRules() string
func (b *Bindings) SetSystemRules(content string) error
```

Both delegate through the agent (`agent.SystemRules()` /
`agent.SetSystemRules(content)`) which acquires `a.mu`,
manipulates the Store, and returns. Hot reload is automatic:
the next turn calls `a.sysRules.Get()` from inside
`buildMessagesV2` and the new content flows into
`BuildSystemPrompt` as the fourth argument.

The "save state must go through the agent layer" rule from
`feedback_in_memory_disk_sync` applies: bindings never touch
`sysrules.Store` directly.

### 3.5 Token estimation

The Settings UI shows a live "~N tokens" advisory in the
textarea footer. We approximate locally in the frontend with
`Math.max(content.length/4, words*1.3)` — close enough to the
backend `memory.EstimateTokens` algorithm
(`feedback_token_estimation_json`: `max(chars/4, words×1.3)`)
for an advisory and avoids an RPC round trip per keystroke.

### 3.6 Settings UI

New section in `frontend/src/components/settings/`, sibling to
the existing memory / sandbox / tools sections:

```
┌─ System Rules ─────────────────────────────────────┐
│ Standing instructions the agent follows in every   │
│ session. Plain Markdown. Saved to:                 │
│ ~/Library/Application Support/shell-agent-v2/     │
│   system_rules.md                                  │
│                                                    │
│ ┌──────────────────────────────────────────────┐  │
│ │ <textarea, monospace, ~12 rows visible>      │  │
│ │                                              │  │
│ └──────────────────────────────────────────────┘  │
│                                                    │
│ 1,247 chars · ~310 tokens     ⚠ none / advisory   │
│                                                    │
│           [ Reload from disk ]  [ Save ]          │
└────────────────────────────────────────────────────┘
```

- **Reload from disk**: re-reads the file (useful after external
  editor changes; otherwise the textarea state is the truth
  during editing).
- **Save**: writes the file and propagates to the engine.
- **Token advisory**: green when `< 5%` of active backend's
  `MaxContextTokens`, yellow `5%–20%`, red `> 20%`. These
  thresholds are guidance, not enforcement — system rules are
  user-intentional; we don't refuse to save a long file.

The textarea has IME-composition handling identical to the
ChatInput / Sidebar / MITLDialog pattern documented in v0.6.6.

### 3.7 No migration

There is nothing to migrate from. v0.6.x had no equivalent
facility. New install or upgrade-from-v0.6.x both start with
`system_rules.md` absent → equivalent to empty rules → no
behaviour change until the user authors content.

---

## 4. Testing

### 4.1 `sysrules.Store` unit tests

- `Load` on missing file returns nil error, `Get` returns `""`.
- `Save` then `Load` round-trip preserves content (with CRLF →
  LF normalisation verified).
- `Save` is atomic: simulate a panic mid-write by writing to a
  read-only directory; verify the target file is unchanged.
- Concurrent `Save` + `Get` from goroutines: no race detector
  trips.
- Trailing-newline normalisation: input `"a\nb"` → file content
  `"a\nb\n"`; input `"a\nb\n\n"` → file content `"a\nb\n"`.

### 4.2 `chat.BuildSystemPrompt` injection

Three parametric cases:

- Empty rules: prompt contains no `<system_rules>` marker, no
  preamble sentence. Byte-exact unchanged from current behaviour.
- Short rules: marker present, content between tags equals input,
  preamble sentence present, position is after base prompt and
  before temporal context (assert by substring offset ordering).
- Multi-paragraph rules with surrounding whitespace: input is
  TrimSpace'd by `SetSystemRules` before injection (no leading or
  trailing newline inside `<system_rules>` block).

### 4.3 Bindings round-trip

`bindings_test.go` — call `SetSystemRules("…")`, then
`GetSystemRules()` returns the same content. Subsequent
`BuildSystemPrompt` call includes the new content (verifies the
agent propagation path).

### 4.4 Structural test

A new structural test in `internal/chat/chat_structural_test.go`
(or co-located with existing structural tests) that asserts the
ordering invariant: in a non-empty-rules prompt, the substring
offset of `<system_rules>` is greater than the offset of the
base prompt's first line, and less than the offset of the
`Current date is` line (temporal context). Mechanical guard
against future drift moving the injection position.

### 4.5 Token estimator parity

`sysrules.EstimateTokens` returns the same value as
`memory.EstimateTokens` for a shared corpus (since it's a
re-export). One sanity test.

---

## 5. Risks

- **Prompt-injection via rules content.** System Rules content is
  *trusted* — it came from the user, not from data. The
  `<system_rules>` envelope is for clarity, not defense. If a
  malicious actor edits the file directly on disk, they already
  have filesystem-level access and the threat model is broken
  outside this feature's scope. **Mitigation**: explicit doc
  language that system rules are read with full trust.
- **Token budget exhaustion.** A very long rules file could
  consume most of the context window on the local backend
  (default `MaxContextTokens` 16,384). **Mitigation**: Settings
  UI shows the percentage advisory; we do not silently
  truncate (truncation of user-authored instructions would be
  surprising). User pays the cost intentionally.
- **Conflict with base system prompt or sandbox guidance.** A
  user could write rules that contradict the base prompt or
  sandbox guidance ("never use tools", "always use sandbox X").
  **Mitigation**: the preamble sentence frames system rules as
  "override default behaviour *unless* they conflict with safety
  or security guidelines", which gives the LLM permission to
  disregard a rule that would block its core function. We accept
  that users can shoot themselves in the foot; this is opt-in
  configuration.
- **Hot-reload race on Save during in-flight turn.** Save in
  Settings while a turn is running cannot race against the
  prompt assembly: `BuildSystemPrompt` receives `systemRules` as
  a function parameter (a string value snapshotted from the
  agent at turn entry), not from a shared `Engine` field. The
  in-flight turn keeps using its snapshot; the next turn snapshots
  fresh. **No mitigation needed beyond Agent's `a.mu` already
  serialising Store reads and writes.**

---

## 6. Rejected alternatives

### 6.1 Store rules inside `config.json` as a string field

`Config.Agent.SystemRules string`. **Rejected**: long Markdown in
JSON is hostile to edit (escaped newlines, no syntax
highlighting); a future config schema change risks corruption;
the AGENTS.md / CLAUDE.md mental model is "a file you author",
not "a config knob". Independent file is the right shape.

### 6.2 Pin a special-category item into Global Memory

Treat system rules as a Global Memory entry with category
`"rule"`. **Rejected**: Global Memory's promotion logic, UI, and
extraction pipeline assume "things the agent learned about the
user". A user-authored standing instruction is the opposite
direction — declarative, not learned. Forcing both into one
facility muddies the model and the UI.

### 6.3 Project-directory `AGENTS.md` auto-discovery

Walk up from a "current project directory" and concatenate
discovered `AGENTS.md` files, the way Claude Code does for
`CLAUDE.md`. **Rejected for v0.7.0**: shell-agent-v2 is a GUI
chat application with no "current project directory" concept.
Adding one is a bigger product question (multi-workspace,
project switching, scope of file inheritance). System Rules as
a single global file solves 95% of the value at a fraction of
the complexity. Could be layered later.

### 6.4 Inject at the end of the system prompt

End-of-prompt placement is sometimes claimed to improve LLM
adherence. **Rejected**: putting user instructions *after*
sandbox guidance, memory blocks, and findings makes the
structure harder to reason about (instructions sandwiching
data), and contradicts the `feedback_prompt_injection_position`
principle of "instructions first, data after". Adherence
differences across modern Gemini / OpenAI-compat models are
within noise for this kind of standing-rules content.

### 6.5 Inject as the first user message in every session

Prepend rules as a faux user turn. **Rejected**: pollutes the
chat transcript, eats Records history, doesn't compose with the
base prompt's role-setting, and is hard to revoke without
rewriting history. The system-prompt position is the natural
home.

### 6.6 Per-session rule overrides in v0.7.0

A "session profile" facility that layers session-specific rules
on top of global. **Rejected for v0.7.0**: not in the requested
scope, adds a second editor surface, and complicates the
"what's active right now" mental model. Trivially additive later
if demand exists — store would gain a per-session file under
`sessions/<id>/system_rules.md`, engine would concatenate, UI
would gain a per-session tab.

---

## 7. Compatibility & rollout

- **Persistence format**: additive — new file `system_rules.md`
  in `<dataDir>`, no changes to existing JSON state files.
- **Config schema**: no changes to `config.json`. The Settings
  UI section talks to `sysrules.Store` directly via dedicated
  bindings.
- **LLM-observable**: when the file exists and is non-empty, the
  system prompt gains a `<system_rules>…</system_rules>` block
  near the top. Empty / missing file → byte-exact unchanged from
  v0.6.6 (verified by test 4.2 case 1).
- **UI-observable**: new Settings section. No changes to chat
  pane, sidebar, or any existing dialog.
- **Import / Export**: session import/export is per-session
  content. System Rules are global. **Out of scope** for the
  bundle format; not exported. (Could be added to the bundle
  later under an opt-in flag if requested; deliberately deferred.)
- **Rollout**: ships as v0.7.0. CHANGELOG calls out the new
  facility with usage example. README and README.ja gain a brief
  System Rules subsection; full reference doc at
  `docs/{en,ja}/reference/system-rules.md`.

---

## 8. References

- Reported by user: 2026-05-15, "グローバルメモリとは別に、システム
  設定として Shell-Agent が従うべきルールのようなもの設定で定義
  できるような機能を追加したい (AGENTS.md みたいなかんじ)".
- `feedback_prompt_injection_position` — defense instructions go
  at the top of the prompt. Generalised here to "all trusted
  instruction surface before any data surface".
- `feedback_in_memory_disk_sync` — per-session state must go
  through the agent layer, not bindings → memory directly.
  Applied here to the engine `systemRules` field path even
  though it's global rather than per-session.
- `feedback_token_estimation_json` — word-count under-estimates
  JSON / Markdown by 4-5×; re-use the `max(chars/4, words×1.3)`
  estimator already in `internal/memory`.
- Existing memory model: `docs/en/reference/memory-model.md` —
  the four facilities (Records, Session Memory, Findings, Global
  Memory). System Rules is **not** a fifth memory facility; it
  is a *configuration* surface that happens to flow into the
  same destination (system prompt). The docs will be explicit
  about this categorisation.
- Affected sites (new in this ADR):
  - `app/internal/sysrules/` (new package — Store, no internal mutex)
  - `app/internal/config/config.go` (`SystemRulesPath()` helper)
  - `app/internal/chat/chat.go` (`BuildSystemPrompt` gains a 4th
    `systemRules` parameter; no new engine field)
  - `app/internal/agent/agent.go` (`sysRules *sysrules.Store`
    field, load on init, `SystemRules()` / `SetSystemRules()`
    methods, `buildMessagesV2` passes the snapshot as the 4th
    argument)
  - `app/bindings.go` (`GetSystemRules`, `SetSystemRules`)
  - `app/frontend/src/dialogs/SettingsDialog.tsx` (new "rules" tab)
  - `app/internal/chat/chat_test.go` (BuildSystemPrompt cases)
  - `app/internal/sysrules/sysrules_test.go` (new)
  - `app/bindings_test.go` (round-trip)
