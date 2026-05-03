# Shell Tool Execution Timeout — Design Document

> Date: 2026-05-02
> Status: Proposed for the next release
> Scope: Add a per-tool `@timeout: N` script header so individual
> shell tools can opt out of the package-default 30-second
> execution cap. Backward-compatible additive change.

## 1. Background

`internal/toolcall/toolcall.go:17` hard-codes the per-tool execution
timeout:

```go
const DefaultTimeout = 30 * time.Second
```

`Execute()` wraps every shell tool invocation in
`context.WithTimeout(ctx, DefaultTimeout)`. The cap exists to stop a
runaway / hung script from blocking the agent loop indefinitely; for
the bundled tools (`weather`, `list-files`, `write-note`,
`get-location`, `file-info`, `preview-file`) 30 seconds is generous.

The user reports a real shell tool that legitimately takes longer
than 30 seconds — likely a script that polls an external service or
runs a heavy local command — and the cap forces a `context deadline
exceeded` error before the script finishes.

## 2. Why now

The user has actually written such a tool. There's no point keeping
the cap hidden until they file an issue against it.

## 3. Goals / Non-goals

### Goals

1. Let a script declare its own timeout via a header directive,
   processed at registration time alongside the existing
   `@tool` / `@description` / `@param` / `@category` headers.
2. Keep the 30-second `DefaultTimeout` as the package-wide
   fallback. Existing scripts (bundled and user-customised) MUST
   continue to work unchanged.
3. Validate the parsed value defensively — reject non-numeric or
   negative values rather than silently misinterpreting them.

### Non-goals

- **No global "all tools default to N seconds" config.** The user
  explicitly questioned the UX of "edit config.json" for a GUI
  app, and surfacing this as a Settings UI knob is overkill while
  per-tool override covers the actual use case. If multiple
  long-running tools accumulate later, add a Settings → Tools
  field then; the additive nature of this change keeps that path
  open.
- **No floor or ceiling on the override.** The user is the script
  author; if they want 1 second or 1 hour, that's their call.
  (We do reject zero / negative as an obvious format error.)
- **No "infinite timeout" sentinel.** Every script must complete
  in some bounded time; the agent loop assumes bounded calls.
  Want longer? Just say `@timeout: 7200`.

## 4. Detailed design

### 4.1 Header syntax

```
#!/bin/bash
# @tool: heavy-poll
# @description: Poll the slow external service
# @param: url string "Endpoint to poll"
# @category: read
# @timeout: 120
```

- Key: `@timeout:`
- Value: positive integer, **seconds**
- Whitespace around the value is trimmed
- Anything that doesn't parse as a positive integer is logged at
  warn level and the script falls back to `DefaultTimeout`. No
  registration failure (the script is still loaded and usable;
  we just don't trust the header).
- Decimal seconds (`@timeout: 0.5`) and Go duration strings
  (`@timeout: 90s`) are NOT supported in this round — the JSON
  field is `time.Duration` (nanoseconds), but the user surface
  is "integer seconds" for clarity.

### 4.2 Tool struct

```go
type Tool struct {
    Name        string        `json:"name"`
    Description string        `json:"description"`
    Params      []Param       `json:"params"`
    Category    Category      `json:"category"`
    ScriptPath  string        `json:"script_path"`

    // Timeout, when > 0, overrides the package-level DefaultTimeout
    // for this tool only. Zero (the default) means use DefaultTimeout.
    // Set via the `@timeout: N` script header (N in seconds).
    Timeout     time.Duration `json:"timeout,omitempty"`
}
```

The field is `time.Duration` so internal call sites stay typed
(no scattered `int * time.Second` arithmetic). JSON is `omitempty`
so registry serialisation looks the same as before for scripts
without an override.

### 4.3 Execute()

```go
func Execute(ctx context.Context, tool *Tool, argsJSON string) (string, error) {
    timeout := tool.Timeout
    if timeout <= 0 {
        timeout = DefaultTimeout
    }
    ctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()
    ...
}
```

One-line conditional, no behaviour change for scripts without an
override.

### 4.4 Header parser

In `parseToolHeader`, after the existing `@param` branch:

```go
} else if strings.HasPrefix(line, "@timeout:") {
    raw := strings.TrimSpace(strings.TrimPrefix(line, "@timeout:"))
    if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
        tool.Timeout = time.Duration(secs) * time.Second
    } else {
        // Logged once at registration; script still loads with
        // DefaultTimeout.
        logger.Error("toolcall: %s: ignoring invalid @timeout %q (must be a positive integer of seconds)", path, raw)
    }
}
```

Uses `internal/logger` to keep the app's log path single-channeled
(per project policy — no second log destination). `logger.Error`
rather than `Info` because an invalid header is a user-fixable
mis-configuration that should be visible without trawling the
verbose log stream.

Adds `internal/logger` as an import from `internal/toolcall`. No
cycle risk: `logger` has no internal dependencies. Tests for the
parser remain hermetic — `logger.Error` is a no-op in unit tests
unless `logger.Init` was called.

### 4.5 Tests

- `TestParseToolHeader_TimeoutHonoured` — header with
  `@timeout: 90`, expect `tool.Timeout == 90s`.
- `TestParseToolHeader_TimeoutMissing_DefaultsToZero` — no
  header line, `tool.Timeout == 0` (Execute falls back to
  DefaultTimeout).
- `TestParseToolHeader_TimeoutInvalid_FallsBack` — `@timeout: abc`,
  `@timeout: 0`, `@timeout: -10` all leave `tool.Timeout == 0`.
- `TestExecute_HonoursToolTimeout` — write a temp script that
  `sleep 2`, set `tool.Timeout = 500ms`, expect timeout error.
- `TestExecute_FallsBackToDefaultTimeout` — same script but
  `tool.Timeout = 0`; `DefaultTimeout` (30s) should let the 2s
  sleep complete normally.

### 4.6 Bundled + example script timeouts

All shipped scripts get explicit `@timeout` declarations so future
script authors discover the option by example. Per-script values
chosen to reflect realistic worst-case runtime:

| Script | Type | `@timeout` | Rationale |
|---|---|---|---|
| `bundled/tools/weather.sh` | bundled | `30` | JMA XML fetch; 30s is plenty |
| `bundled/tools/get-location.sh` | bundled | `30` | IP geolocation; fast |
| `bundled/tools/list-files.sh` | bundled | `30` | local FS walk; fast |
| `bundled/tools/file-info.sh` | bundled | `30` | local stat; fast |
| `bundled/tools/preview-file.sh` | bundled | `30` | local read + format; fast |
| `bundled/tools/write-note.sh` | bundled | `30` | local write; fast |
| `bundled/tools/examples/web-search.sh` | example | **`120`** | Calls `gem-search` which makes a Vertex AI Gemini grounded-search round-trip — frequently > 30s |
| `bundled/tools/examples/generate-image.sh` | example | **`120`** | Calls `gem-image` which makes a Vertex AI image-generation round-trip — frequently > 30s |

For the bundled six, the `30` value matches `DefaultTimeout` and
is therefore strictly redundant — but having it spelled out makes
the option discoverable at a glance, and removes the special-case
"if you don't see @timeout, it's actually 30" rule from the
reader's head.

For the two example scripts, the explicit raise to `120` seconds
fixes a real footgun: users pulling these into their own
deployments would otherwise hit `context deadline exceeded` on
typical agentic-search / image-gen calls.

## 5. Touched files

| File | Change |
|---|---|
| `internal/toolcall/toolcall.go` | `Tool.Timeout` field; `parseToolHeader` `@timeout` branch; `Execute` per-tool override |
| `internal/toolcall/toolcall_test.go` (new) | parser + Execute tests |
| `internal/bundled/tools/weather.sh` + 5 other bundled scripts | Add `@timeout: 30` (matches default; spelled out for discoverability) |
| `internal/bundled/tools/examples/web-search.sh` | Add `@timeout: 120` (gem-search frequently exceeds 30s) |
| `internal/bundled/tools/examples/generate-image.sh` | Add `@timeout: 120` (gem-image likewise) |
| `CHANGELOG.md` | `[Unreleased]` Added entry |
| `AGENTS.md` | Gotcha: per-tool timeout override |
| `README.md` / `README.ja.md` | Section in shell-tool docs |

## 6. Backward compatibility

| Surface | Pre-change | Post-change | Compat |
|---|---|---|---|
| Scripts without `@timeout` | 30s cap | 30s cap | ✅ identical |
| External callers of `Execute()` | takes `(ctx, tool, args)` | unchanged signature | ✅ |
| External code reading `Tool{}` | 5 fields | 6 fields (additive) | ✅ |
| `parseToolHeader` known directives | 4 (`@tool`, `@description`, `@param`, `@category`) | 5 (`+@timeout`) | ✅ unknown directives already silently ignored |
| JSON serialisation of `Tool{}` | no `timeout` key | `timeout` key only when > 0 (`omitempty`) | ✅ |

No on-disk format change, no migration needed.

## 7. Risks & mitigations

| Risk | Mitigation |
|---|---|
| User typos `@timeout: 5min` (Go duration string) and the script silently uses DefaultTimeout | Log a warning at registration time so the typo is visible in `app.log` |
| User sets `@timeout: 1` thinking ms, gets killed at 1s | Document clearly in README + bundled-script comment that the unit is seconds |
| Long-running script blocks the agent loop while the LLM call also waits | Already true under DefaultTimeout (any tool taking 25s holds up the loop). Per-tool timeout is just a longer leash; same mechanism. |

## 8. Out of scope

- A `tools.default_execution_timeout_seconds` global config knob.
  Easy to add later when needed.
- A Settings UI field for the global default. Same — defer.
- Decimal seconds / Go duration strings. Defer until someone
  needs sub-second precision.
- Per-tool MITL prompt timeout. Different concern.
