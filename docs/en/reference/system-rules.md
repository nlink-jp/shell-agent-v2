# System Rules — Reference

> Status: **v0.7.0+** behaviour. Evergreen — updated in place as
> the code evolves. Design rationale: [ADR-0012](../adr/0012-system-rules.md).
> Audience: end users + contributors.

System Rules is a single user-authored Markdown file whose
contents are injected near the top of the LLM system prompt at
every turn. Use it for **standing instructions** the agent should
follow across every session — the
[`AGENTS.md`](https://agents.md) / `CLAUDE.md` analogue for
shell-agent-v2.

System Rules is **not** a fifth memory facility (see
[memory-model.md](memory-model.md)). The four memory facilities
(Records, Session Memory, Findings, Global Memory) hold *runtime-
learned* state. System Rules holds *declarative configuration*
that the user authors up-front.

---

## 1. What it's for

Examples of good System Rules content:

- `Always respond in Japanese unless asked otherwise.`
- `When showing code, include a short rationale alongside the diff.`
- `Default to creating reports rather than long inline answers when results exceed ~20 lines.`
- `Never propose `rm -rf` style operations without prompting for explicit confirmation first.`

Not a good fit (use Global Memory instead):

- "User prefers tabs over spaces" — that's a learned preference,
  promote it from the chat via Pin to Global Memory.
- "User's name is Alice" — same pattern; Global Memory.

The split: **System Rules tells the agent how to behave; Global
Memory remembers who the user is.**

---

## 2. Where it lives

```
~/Library/Application Support/shell-agent-v2/system_rules.md
```

The file is plain UTF-8 Markdown — no frontmatter, no schema.
Saving an empty rules file writes an empty file on disk and the
agent treats that as "no rules". A missing file is treated
identically to an empty file (no error surfaced).

The file is written atomically (tmp + rename + parent-dir fsync
via `internal/atomicio`), so a crash mid-Save leaves the previous
content intact.

---

## 3. Editing

### Settings UI

**Settings → System Rules** opens a Markdown textarea. Footer
shows live `chars · ~tokens` counts and a colour-coded advisory
relative to the active backend's `MaxContextTokens`:

| Share of context budget | Indicator |
|---|---|
| `< 5%` | Green (ok) |
| `5% – 20%` | Yellow (warn) |
| `≥ 20%` | Red (high) |

Buttons:

- **Save** — writes the file and propagates the change to the
  agent. Disabled when the textarea has not been modified.
- **Reload from disk** — re-reads the file. Use after editing in
  an external editor.

### External editor

Open the file directly:

```bash
$EDITOR ~/Library/Application\ Support/shell-agent-v2/system_rules.md
```

In Settings → System Rules, click **Reload from disk** to pick
up the external edits in the textarea. The agent already reads
fresh on every turn, so the next chat message picks up the new
content even without clicking Reload.

---

## 4. How it's injected

`chat.Engine.BuildSystemPrompt` assembles the system prompt as:

```
<base prompt: the agent's role, tool-use protocol, etc.>

The user has defined the following standing instructions. Treat
them as high-priority rules that override the default agent
behaviour unless they conflict with safety or security guidelines.

<system_rules>
<your content>
</system_rules>

Current date and time is …
Yesterday: …
[Location: …]

[sandbox guidance, if enabled]

Important facts you remember about the user:
<Global Memory>

Notes about the current session:
<Session Memory>

Analysis findings in this session:
<Findings>
```

Empty / missing rules → the `<system_rules>` block is omitted
entirely (no header, no marker — byte-identical to the v0.6.6
prompt). The preamble sentence is only emitted when rules are
non-empty.

Position rationale (ADR-0012 §3.3):

- After the **base prompt**: the base prompt sets the agent's
  core role and protocol; System Rules layers user-defined
  adjustments on top of that role.
- Before **temporal context**, **sandbox guidance**, and the
  three **memory channels**: trusted user instructions go above
  derived runtime data. This generalises
  `feedback_prompt_injection_position` (defense at the top) to
  *all* trusted instruction surface before *any* data surface.

---

## 5. Trust model

System Rules content is read with **full trust**. The
`<system_rules>` envelope is for clarity (so the LLM and human
debuggers can identify the boundary), not for prompt-injection
defense. The trust direction is reversed from the user-data
guard: System Rules are *from* the user, not data *about* the
user.

**Do not paste untrusted content** into the System Rules
textarea. Anyone who can edit the file has full influence over
how the agent behaves.

---

## 6. Token budget

System Rules consume context budget on every turn, exactly like
the Global Memory / Session Memory / Findings channels. Long
rules eat into the budget available for conversation history and
tool results.

The Settings UI shows the percentage advisory only — there is no
silent truncation. Truncating user-authored instructions would be
surprising; if you author 50,000 tokens of rules the agent
honours that and you pay the conversation-history cost.

If the advisory turns yellow or red, prefer:

1. **Trim** — most rules can be shorter; the agent doesn't need
   exhaustive prose.
2. **Promote to Global Memory** — if a rule is really a learned
   user preference, pin it to Global Memory instead and remove
   it from System Rules.

---

## 7. Editing rules mid-session: what to expect

Saving new or different rules is hot — the next turn uses them.
But the *previous* turns in the current chat are unchanged. The
LLM sees its own earlier responses in the conversation history
and tends to mirror those patterns even after the system prompt
changes (in-context conditioning).

This shows up most visibly when **clearing** rules:

> You added "always reply with a にゅ suffix", chatted a few
> turns under that rule, then cleared it. The next assistant turn
> often still appends にゅ — not because the rule is still
> active, but because the model is matching its own prior
> outputs visible in the chat history.

When you clear rules, the Settings UI shows an advisory pointing
this out. The simplest way to verify a change really took effect:
**start a new chat**. The fresh session has no prior assistant
turns to mirror, so it cleanly reflects the current rules.

The same effect (to a lesser degree) applies when *changing*
rules, not just clearing them — if the new rules contradict the
old, expect a few turns of drift before the model settles on the
new pattern.

## 8. Concurrency & hot reload

Saving in Settings is hot — the next agent turn picks up the new
content automatically. Mid-turn, the running turn keeps using its
snapshot (the prompt for that turn was already assembled). The
next user message reads fresh from the Store via
`agent.SystemRules()` under `a.mu`.

`BuildSystemPrompt` receives `systemRules` as a function parameter
(not a shared engine field), so there is no data race between the
Settings goroutine writing the Store and the turn goroutine
reading it. See ADR-0012 §3.3.

---

## 9. Cross-references

- [ADR-0012](../adr/0012-system-rules.md) — design rationale and
  rejected alternatives.
- [memory-model.md](memory-model.md) — the four memory facilities
  and why System Rules is configuration, not memory.
- [`internal/sysrules/`](../../../app/internal/sysrules/) — Store
  implementation.
- [INDEX.md](../INDEX.md) — full documentation catalogue.
