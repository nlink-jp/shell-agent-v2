# Guard nonce stability for KV cache reuse — Design Note

**Status:** Proposed (2026-05-20).
**Target:** v0.13.1 (patch — addresses a regression-class
performance bug discovered after v0.13.0 release; user-visible
fix with no schema migration and no Wails binding signature
changes).
**Reported by:** User — "tried v0.13.0 in production with local
LM Studio; couldn't tell if the optimisation was effective".
Log inspection showed turns at 27-28 s each across runs 1-3 of a
fresh session, identical to pre-v0.13.0 behaviour.

This note specifies a **scan-and-rotate** policy for the guard
nonce used by `nlk/guard` to wrap untrusted user / tool content.
v0.13.0 rotated the nonce on every `BuildSystemPrompt` call,
which silently invalidated KV cache reuse for the entire
conversation history — defeating ADR-0017 in production. The
proposed policy holds the nonce stable for the session and only
rotates it when the current nonce string actually appears inside
untrusted record content (the precise condition under which a
nonce-leak prompt injection becomes possible).

The bench harness ([T10a / T10b / T10c](../history/llm-cache-bench-2026-05-20.md))
shows the design recovers the ~93 % speedup ADR-0017 targeted,
with a single-turn cost on rare rotation events.

---

## 1. Problem

ADR-0017's `BuildSystemPrompt` rewrite removed the per-call
volatile timestamp from the system prompt, so the system block
is byte-identical across consecutive requests within a session.
The bench harness's T8 confirmed a 96 % speedup at the
production prompt size.

Production tests on real LM Studio showed no speedup at all.
The log:

```
Turn 1: duration=28.0 s, prompt 14976 tokens
Turn 2: duration=27.3 s, prompt 15089 tokens
Turn 3: duration=27.9 s, prompt 15252 tokens
```

The cause is `internal/chat/chat.go:138`:

```go
func (e *Engine) BuildSystemPrompt(...) string {
    e.guardTag = guard.NewTag()  // ← rotates per call
    ...
}
```

The new tag's name is a cryptographic nonce (16 hex bytes:
`user_data_<32 chars>`). Subsequent rendering in
`contextbuild.renderRecordContent` wraps each user / tool
record's content with `<user_data_XXX>...</user_data_XXX>`. The
nonce changes every turn, so:

- Turn 1 sends `<user_data_AAA>hello</user_data_AAA>` for user
  record #1.
- Turn 2 sends `<user_data_BBB>hello</user_data_BBB>` for the
  same user record #1.

The byte prefix of turn 2's full prompt diverges from turn 1's
cache immediately after the system block (which is the only
stable bytes). KV cache reuse cannot extend through the
conversation history — which dominates the prompt at 15 K
tokens.

**Bench evidence** ([report](../history/llm-cache-bench-2026-05-20.md)):
- T10a (per-turn nonce, current production) → 1545 ms/turn flat,
  0 % speedup.
- T10b (per-session-stable nonce, this ADR's normal case) →
  106 ms/turn after the first, **93 % speedup**.
- T10c (rotate-on-detection at turn 3) → cache-warm except for
  the one rotation turn (~1500 ms), then cache-warm again.

### Why per-turn rotation was put there in the first place

`nlk/guard`'s godoc warns:

> A new Tag must be generated for every LLM call (turn).
> Reusing the same Tag across multiple turns is unsafe because
> a previous LLM response may echo the tag name, allowing prompt
> injection in subsequent turns.

The attack sequence:

1. The LLM, in its response, includes the literal tag name
   (e.g. mentions `<user_data_AAA>` in prose).
2. The user (or attacker authoring the content the user pastes)
   sees the leaked nonce — directly in the response, or
   indirectly via logs / network.
3. The attacker crafts the next user input to contain
   `</user_data_AAA>`, prematurely closing the wrap. Content
   after the synthetic close is positioned in the system-
   instruction zone.

Per-turn rotation guarantees the nonce used to wrap turn N+1's
content was unknown at turn N — even if the LLM echoed turn N's
nonce, the attacker can't predict turn N+1's.

The defence is sound, but its mechanism (rotate every call) is
much stronger than the actual attack window requires.

---

## 2. Goals

1. **KV cache prefix reuse fires for the conversation history
   in normal operation.** Recover ADR-0017's 93 % wall-clock
   saving on local LLM turns.
2. **No effective weakening of prompt-injection defence.** The
   nonce must still be unpredictable to an attacker who hasn't
   seen it leaked, and the wrap must still resist a
   leaked-nonce attack.
3. **No schema migration, no Wails binding signature change.**
   The fix is internal to the chat / contextbuild plumbing.

Non-goals:

- **Replacing the guard wrap with a different defence
  mechanism.** Out of scope; the wrap is the established
  pattern and works.
- **Cross-session nonce reuse.** Each session gets its own
  nonce. Per-process or global static nonce is rejected (§5.2).
- **Detecting LLM-side nonce echo in the assistant's response.**
  The decisive moment is when the leaked nonce actually appears
  in untrusted (user / tool) content — that's the only place an
  attack can land. Detecting LLM echo earlier would be eager
  rotation that doesn't add safety.

---

## 3. Design

### 3.1 Threat-model framing

The attack succeeds only when **two events both happen**:

1. The LLM echoes the current nonce string into a context the
   attacker can read.
2. The attacker places the same nonce string (typically as
   `</user_data_XXX>`) into untrusted content fed back into the
   model.

Either event alone is benign. (LLM echo by itself reveals the
nonce but doesn't inject; attacker text containing a string
they can't predict can't escape the wrap.)

The current per-turn rotation guards against (2) by making the
nonce unpredictable on every call, even if (1) happened on the
prior turn. But the same outcome is achieved by detecting
event (2) directly — looking for the current nonce inside
untrusted content right before we use it — and rotating
*only at that moment*.

The detection is precise (a 16-byte hex string has zero
false-positive risk for benign user prose), and the rotation
window is exactly when the threat materialises.

### 3.2 Algorithm

```go
// In chat.Engine (or wrapped around the existing guardTag
// rotation point):
//
//   PrepareWrap(session *memory.Session) — call before
//   BuildSystemPrompt / contextbuild.Build for each LLM round.
//
// Behaviour:
//   1. If the current guardTag is empty (first build of the
//      session), generate one and return.
//   2. Scan every user / tool record's stored content for the
//      current nonce string (as a plain substring, or
//      conservatively the open / close tag forms).
//   3. If any record contains it, generate a fresh nonce.
//   4. Otherwise, keep the current nonce.
//
// Postcondition: all records rendered in this build use the
// same nonce — either the surviving one or the freshly-rotated
// one. There is no mixed-nonce output within a single build.
```

The rotation is **lazy**: it happens only when actually needed.
In a clean session with no nonce leak, the nonce persists across
turns and the prompt prefix is byte-stable.

### 3.3 What to scan for

A guard tag has a name like `user_data_a1b2c3d4e5f6...`. The
attacker's payload uses `</user_data_a1b2c3d4e5f6...>` to escape
the wrap. A conservative scan checks for any of:

1. `<` + name + `>` (opening tag) — would only appear in
   benign content astronomically rarely.
2. `</` + name + `>` (closing tag) — same.
3. `name` (substring only, without angle brackets) — also rare.

We scan for all three, conservatively. The cost is a few O(n)
`strings.Contains` calls per record; negligible compared to
tokenisation or LLM round-trips.

### 3.4 Where to scan

Untrusted content lives in:

- `Record.Content` for `Role == "user"` (typed input, paste,
  drag-drop attachments converted to text).
- `Record.Content` for `Role == "tool"` (tool output that may
  contain web fetches, shell results, etc.).

We do NOT scan:

- Assistant records — they're the LLM's output; if the model
  echoed the nonce, it lands here. We don't rotate solely on
  echo (that's not a threat by itself), but the next user
  record's `Content` will trigger rotation if the user copy-
  pastes the assistant message.
- The system prompt — even if a leaked nonce ends up baked
  into a memory block (rare, via fact extraction from
  attacker-controlled user content), the model reading it is
  not the attack surface. The next user input is.

### 3.5 Rotation atomicity within a build

`PrepareWrap` does the scan **once, up front**, before any
record is rendered. If it rotates, every subsequent
`WrapUserToolContent` call in this build uses the new nonce.
The build's output is internally consistent (no mixed nonces).

The next build (next round / next turn) runs `PrepareWrap`
again. If the new content (e.g. a tool result) contains the
new nonce, another rotation fires. In practice this is
exceptionally rare.

### 3.6 Where to wire it

```
agent.buildMessagesV2
  ├─ a.chat.PrepareWrap(a.session)        ← new
  ├─ systemPrompt := a.chat.BuildSystemPrompt(...)
  │     (no longer rotates internally — moved to PrepareWrap)
  └─ contextbuild.Build with WrapUserToolContent unchanged
```

The mutation moves from `BuildSystemPrompt` into the new
`PrepareWrap`. `BuildSystemPrompt` becomes guard-tag-agnostic —
it just uses the current tag.

### 3.7 Effects on the historical "shouldAnnotate" workaround

`contextbuild/render.go:31-41`'s `shouldAnnotate` comment notes
a separate gemma-2.5 historical-event misread that was tied to
the system-block temporal context. ADR-0017 already eliminated
that condition by removing the temporal context from the system
prompt. ADR-0018's nonce stability does not re-introduce it.

---

## 4. Edge cases

1. **First build of the session.** `e.guardTag` is empty;
   `PrepareWrap` generates the initial nonce. Same as today's
   first turn.

2. **Multi-round agent loop within a single SEND.** Each
   round calls `buildMessagesV2`, so `PrepareWrap` runs each
   round. Normally no rotation fires between rounds (the only
   content added between rounds is a tool result; the tool
   result wouldn't contain the nonce unless something extreme
   happened). Rounds within a single SEND therefore reuse cache
   end-to-end.

3. **`/profile` switch mid-session.** The agent rebuilds its
   backend client; LM Studio's server-side state may or may not
   persist depending on profile pointing to the same endpoint.
   The nonce we hold doesn't matter — if the server cache
   resets, the next build is cold regardless. No special
   handling.

4. **Session switch.** `LoadSession` keeps the agent's
   `chat.Engine` (and therefore its `guardTag`) intact. The new
   session's records have no relationship to the previous
   session's nonce. PrepareWrap on the next build either keeps
   the current nonce (no leak in the new session yet) or
   rotates. Either way: correct behaviour. Optional: reset the
   tag on session load so the new session starts cold — keeps
   nonce per-session rather than per-process. Recommended for
   isolation; the cost is one cold turn on session switch.

5. **Tool result happens to contain the nonce string.** Rare
   but possible (e.g. a `sandbox-run-shell` echo of an earlier
   wrapped record). PrepareWrap catches it on the next build,
   rotates. One slow turn, then back to normal.

6. **System-rule edits that include the nonce.** The user edits
   their `system_rules.md`. The rules are part of the system
   prompt, not records. PrepareWrap doesn't scan them. The
   nonce going into the system prompt is "leaked to the LLM"
   but not to the attack surface (next user input). If the user
   later includes the same string in user input, PrepareWrap
   catches it then.

7. **Race between PrepareWrap and concurrent agent activity.**
   Each `buildMessagesV2` call is sequential within an agent
   loop. No concurrency to worry about.

8. **Very long content with many partial nonce-like substrings.**
   `strings.Contains` is O(n); even on a 200 KB tool result the
   scan is sub-millisecond. Not a concern.

---

## 5. Rejected alternatives

### 5.1 Keep per-turn rotation, accept the lost cache win

Rejected. Defeats ADR-0017 entirely in production. The bench
T10a shows zero speedup; production logs confirm 27-28 s/turn
on 15 K-token sessions.

### 5.2 Per-process static nonce (global)

Rejected. Even if no real-world attack chain materialises in
shell-agent-v2's threat model today, persisting a single nonce
across all sessions of all users / all time is unnecessary
breadth. Session-scoped rotation gives the same cache win with
better isolation.

### 5.3 Never rotate (single nonce per app install)

Rejected, same reason as 5.2 plus the obvious "if the nonce ever
leaks, it never recovers" failure mode.

### 5.4 Deterministic nonce derived from session ID

Considered. Computable as `HMAC(session_secret, session_id)`,
stable across reloads of the same session. Pros: replay-safe;
each session has its own nonce; reproducible.

Rejected (for this ADR) because it doesn't address the leak
case — a leaked nonce persists for the session's lifetime and
the attacker has a stable target. The scan-and-rotate design
handles leaks naturally; determinism here adds no safety.

A future ADR could combine `HMAC(session, version)` with a
manual version bump on detection if there's a strong
reproducibility need.

### 5.5 Rotate on detected LLM echo (eager rotation)

Considered. After each assistant response, scan it for the
current nonce. If found, rotate proactively before the next
user input is processed.

Rejected: the scan-and-rotate-on-untrusted-content approach
(this ADR) catches the same threats at the **decisive moment**
— when the nonce actually shows up in attacker-controllable
content. Eager rotation on echo adds a cache miss on every
turn where the LLM happens to mention the wrap structure, even
in benign contexts (e.g. the model explaining how prompt
defence works). Strictly less performant for equivalent
security.

### 5.6 Cryptographic seal instead of plain wrap

Considered. Replace `<nonce>...</nonce>` with a wrap whose
boundaries are detectable by an HMAC or signature, so the
attacker can't forge a closing tag even with the nonce known.

Rejected (for this ADR) — much larger architectural change,
requires the LLM to be trained to recognise the seal, and
breaks compatibility with the existing nlk/guard
implementation. Belongs in a separate ADR if anyone wants to
explore it.

---

## 6. Tests / measurement

### 6.1 Bench validation (already run)

`docs/en/history/llm-cache-bench-2026-05-20.md` T10a / T10b /
T10c quantified the design's effect before the ADR was
approved. No additional bench scenarios needed for the
implementation.

### 6.2 Go tests (new)

- `TestPrepareWrap_FirstCallGeneratesTag` — empty engine
  generates a fresh tag on the first call.
- `TestPrepareWrap_NoLeakKeepsTag` — successive calls on a
  session whose records contain no nonce substring keep the
  same tag.
- `TestPrepareWrap_LeakInUserContentRotates` — a user record
  whose content contains the current tag name triggers a
  rotation. The new tag is different.
- `TestPrepareWrap_LeakInToolContentRotates` — same as above
  for tool records.
- `TestPrepareWrap_LeakInAssistantIgnored` — assistant content
  containing the tag does NOT trigger rotation by itself (per
  §3.4).
- `TestPrepareWrap_ConservativeMatchForms` — content
  containing `<name>`, `</name>`, or bare `name` all trigger
  rotation.
- `TestPrepareWrap_ByteStableNormalCase` — across N
  consecutive calls on the same session (no leak), the
  rendered output of `WrapUserToolContent("hello")` is
  byte-identical every time.
- `TestPrepareWrap_LoadSessionRotates` — switching sessions
  via `LoadSession` resets the tag (defensive isolation).

### 6.3 Production verification

After v0.13.1 ships, repeat the local-LLM smoke test from the
v0.13.0 regression report:

- Open a fresh session on a local profile.
- 3 substantive turns.
- Inspect `app.log` for `llm: local.Chat done duration=...`
  lines.
- Expectation: turn 1 ~10-30 s (cold), turns 2-3 well under
  10 s (cache fires).

If turns 2-3 remain at cold-equivalent durations, the deployed
code is not the one that should be running — check the build
artefact, not the design.

---

## 7. Compatibility

### Non-breaking

- `chat.json` records unchanged.
- `BuildSystemPrompt`'s string output unchanged byte-for-byte in
  the normal case.
- Wails binding signatures unchanged.
- `WrapUserToolContent` external API unchanged.

### Backwards observation

A session opened in v0.13.0 looks identical to one opened in
v0.13.1 from the user's perspective. The only behaviour change
is that local LLM turns become significantly faster.

### Forward observation

The `PrepareWrap` hook is a clean extension point for future
nonce strategies (e.g. ADR-0018 §5.4 deterministic nonce, if
ever wanted) without touching `BuildSystemPrompt` or the wrap
implementation.

---

## 8. Phasing

Single PR. Suggested commits:

1. `feat(chat): PrepareWrap scan-and-rotate guard nonce`
   — adds `Engine.PrepareWrap(session *memory.Session)`,
   removes the `e.guardTag = guard.NewTag()` line from
   `BuildSystemPrompt`. Self-contained chat-package change.
2. `feat(agent): call PrepareWrap before buildMessagesV2`
   — wires the new hook in `agent.buildMessagesV2` and (for
   defensive isolation) `agent.LoadSession`.
3. `test(chat): PrepareWrap byte-stability + leak-detection invariants`
   — the eight test cases from §6.2.
4. `docs: ADR-0018 status update + CHANGELOG v0.13.1 + bench report cross-link`
   — ADR Status → Implemented; CHANGELOG entry.

Each commit is independently buildable and tested.

---

## 9. Out of scope

- Cryptographic-seal alternative to the plain XML wrap
  (§5.6) — a separate ADR if anyone wants to explore it.
- Telemetry / observability around rotation events (a
  `bg-task` or `agent:activity` event when the scan rotates).
  Useful for future tuning but not required for the fix.
- Auditing the `nlk/guard` package's documented guarantees
  vs. this consumer's stricter scan-and-rotate behaviour. The
  godoc warning about per-turn rotation remains accurate for
  callers that don't do their own leak detection; this ADR
  documents the alternative for shell-agent-v2 specifically.
