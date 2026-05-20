# ADR-0022: Minimum-viable decomposition of agent.go

- Status: Implemented in v0.14.3 (2026-05-21)
- Deciders: magi
- Related: Issue #10 (the trigger)

## 1. Context

[Issue #10](https://github.com/nlink-jp/shell-agent-v2/issues/10) reports
that `app/internal/agent/agent.go` is large (3,697 LOC, ~100 functions,
44 → 33 fields after #11) and proposes splitting it into 8 responsibility-
aligned files. After discussion, two facts stood out:

1. **The Issue is metric-driven, not pain-driven.** The trigger was
   "the file is big" — there is no documented case of someone getting
   lost, spending excess time navigating, or making a mistake because
   of file size. No specific cognitive-load failure has been recorded
   in commits, PR reviews, or post-mortems.

2. **Coherent large files are common in Go.** `net/http/server.go`
   sits at ~3,900 LOC and is widely considered a fine read because
   every method is on the same struct, with the same lock discipline,
   playing well-defined roles. `agent.go` has the same shape: 100+
   methods on a single `*Agent` receiver, single mutex, well-defined
   roles inside one cohesive component.

The naïve response to a metric-driven complaint is a metric-driven
fix (8 files, 11 files, whatever brings the maxima below some
threshold). That's the wrong answer when no concrete pain is on
the table — it spends review/migration cost without paying back
in any measurable way and creates extra cross-file friction.

## 2. Decision

Extract **only two files** from `agent.go`, chosen because each
satisfies *both* of the following independence criteria:

1. **Topical orthogonality** — the code does not interleave with the
   Agent's send / loop / lifecycle / FSM core.
2. **Reader-skippability** — someone reading the Agent core does not
   need to scroll past this code or hold it in their head.

The two files:

| New file | Approx LOC | Contents |
|---|---:|---|
| `agent_mcp.go` | ~250 | MCP guardian management (startGuardians, spawnGuardian, validateBinaryPath, validateProfilePath, MCPStatuses, stopGuardians, RestartGuardians, restartGuardian, splitMCPName) + the `MCPStatus` type |
| `agent_extract.go` | ~600 | Memory-extraction algorithm (extractMemories, parseExtractionLine, looksLikeTurnToken, matchFactToUserTurn, detectUserLanguageHint, hasSignificantCJK, extractCJKNgrams, extractKeywords, parseTurnToken, stripGemmaToolCallTags) |

After extraction, `agent.go` drops from 3,697 LOC to **~2,850 LOC**.
Still large by some style guides, but the parts that drop out are
the parts a reader was most likely to want to skip:

- **MCP** is a self-contained subsystem with its own validation,
  spawning, and routing logic. A reader focused on Send / Loop /
  Memory has zero reason to read it.
- **Extraction** is a single algorithmic flow (extractMemories +
  parsing + CJK helpers). Reading agentLoop or postResponseTasks
  shouldn't require scrolling past 600 lines of UTF-8 / Jaccard /
  prompt-formatting code.

## 3. What we explicitly do NOT extract

For each cluster considered and rejected, the reasoning is recorded
so a future revisit doesn't re-litigate from scratch:

- **Handler infrastructure** (~400 LOC). Pattern-heavy but the
  `emit*` / `notify*` methods are short and read more like data
  than logic. Extracting them puts the bindings-bridge in a
  different file from the methods that use it, which is annoying
  more often than helpful. Revisit if a future change makes
  handler registration significantly more complex.

- **Send pipeline** (~400 LOC). Tightly coupled to `agentLoop`
  (the dispatched message becomes a turn there). Reading Send
  alongside the loop is the common case; splitting them creates
  navigation churn for very little payback.

- **Agent loop / executeTool / buildToolDefs** (~700 LOC). This
  IS the core. Extracting it leaves an `agent.go` residual that's
  no longer "the Agent file" — it's "Agent minus the loop", which
  is a weird mental model. Better to keep the loop where the
  struct is defined.

- **Post-response orchestration** (~350 LOC). `postResponseTasks`
  fires from `agentLoop`'s defer; reading them together is the
  natural flow. The auto-dispatch path in particular needs both
  in one window.

- **Memory / Findings CRUD accessors** (~300 LOC). Small,
  shallow methods. Splitting them out of `agent.go` makes
  `bindings.go` -> Agent navigation longer for no clarity gain.

- **Profile / Backend switching** (~450 LOC). Shares state with
  the Send / Loop / Lifecycle paths. Co-locating them means a
  reader debugging a `/model` issue can see the surrounding
  Send code without a tab switch.

- **Tool inspection API** (~300 LOC). Shallow read-side methods
  used by bindings. Same argument as Memory CRUD.

- **Lifecycle (maybeStartSandbox, SetBaseContext/Objects/Analysis,
  LoadSession, sessionWorkDir)** (~250 LOC). LoadSession is
  central — it touches profile, sessionMemory, findings,
  guardians. Co-locating it with the struct + New gives the
  reader the full wiring picture.

## 4. Future revisits

This ADR does NOT commit to "no more decomposition forever." It
commits to "decomposition driven by documented pain, not by
metric alone." If a future contributor reports:

- Specific navigation pain ("I spent 20 minutes looking for X
  and it would have helped to have a separate file")
- Specific contribution pain ("I didn't know where to put a
  new method")
- Specific review pain ("the diff on this PR was unreadable
  because it touched code throughout a 3000-line file")

then a follow-up ADR can extract one or more of the rejected
clusters above. Each future extraction needs to point at the
specific pain it addresses, not at the file's size.

## 5. Implementation

Two commits + release:

1. `refactor(agent): extract MCP guardian management to agent_mcp.go`
2. `refactor(agent): extract memory extraction to agent_extract.go`
3. `chore: release v0.14.3` — patch bump (internal restructure, no
   API change, no behaviour change)

Each extraction is a pure file move: every function lands on the
same `*Agent` receiver, same package, same lock discipline. No
behaviour change. `go test ./... -tags no_duckdb_arrow` must pass
on every commit.

## 6. Closing #10

This ADR's existence + the v0.14.3 release closes Issue #10. A
comment on the issue cites this ADR as the resolution and notes
that the rejected clusters are catalogued in §3 so a future
re-evaluation can build on them rather than starting from scratch.
