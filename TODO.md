# TODO

Tracked items that aren't ready for an implementation pass yet —
either because the design needs more thought, or because the work
is too large to fit into the current release stream.

## Tool-bubble success/failure indication

**Origin**: 2026-04-30 conversation with the user. Idea:
distinguish "tool ran and produced a useful result" from "tool
ran and reported an error" in the chat UI — different colour /
icon on the `tool-event` bubble.

### Why it's not a one-liner

The naive "treat any result starting with `Error:` as failure"
heuristic isn't robust enough. Cases that are likely to fool it:

- **Sandbox shell**: `sandbox-run-shell` returns the container's
  stdout / stderr / exit code as text. A non-zero exit shows
  `[exit: 1]` somewhere in the body but the result string
  itself doesn't start with `Error:`. A failed `pip install`
  looks like normal output to a string-prefix check.
- **Sandbox python**: same — a `Traceback ... ModuleNotFoundError`
  is in the body but the wrapper text isn't `Error:`-prefixed.
  We saw exactly this in v0.1.10 logs (gemma's `pandas` import
  failure).
- **Sandbox export-sql / load-into-analysis**: error path returns
  `Error: ...` from the Go side, but a SQL that runs and produces
  zero rows is not an error and shouldn't be flagged red.
- **MCP tools**: result is whatever the upstream MCP server
  decided, with no contract on prefix.
- **Shell-script tools**: header-defined exit-code semantics; the
  body is whatever the script wrote.

So a string-prefix check would underflag sandbox failures (the
common case) and could overflag false positives elsewhere.

### Design questions to resolve

1. **What does "failure" mean per tool family?** Probably:
   - `executeTool` returns `(string, error)` — error path is
     unambiguous failure. Today most builders return `string`
     only with `"Error: …"` prefixes.
   - Sandbox shell/python: `ExecResult.ExitCode != 0` *or*
     `TimedOut == true`. The dispatcher needs to surface that,
     not just stringify it into the result.
   - MCP: `JSON-RPC error.code != 0` is unambiguous; the stdio
     transport already sees this.
   - Shell-script: `exit_code != 0` from the registry execution.
2. **Where do we carry the status?** Options:
   - Plumb a new field through the `tool-event` runtime event
     (preferred — UI-only concept, no schema break for memory).
   - Or extend the message record itself with a `status` field
     and persist it (more work, broader impact).
3. **Three-state vs two-state?** Running / success / failure is
   the obvious mapping, but consider a fourth: "ran but warning"
   (e.g. exit 0 but stderr non-empty, or `pip install` succeeded
   with deprecation warnings). Keep it to three for now unless a
   real case appears.
4. **Visual treatment** (cheap, do last):
   - success: ✓ in `--text-tool` (existing green token)
   - error: ✗ in `--text-error`
   - running: ● unchanged

### Suggested staging when picked up

1. Audit every `executeTool` branch and classify failure cleanly.
   Refactor sandbox dispatchers to expose ExitCode / TimedOut to
   the event stream instead of swallowing them into the result
   text.
2. Extend the runtime event payload with `status: 'success' |
   'error'`, defaulted to `'success'` for backwards compat.
3. Frontend: extend `tool-event` status union, swap icon + class.
4. Update CSS using existing theme tokens.
5. Ship under the next minor.

Don't implement piecemeal — the value of the indicator depends on
it being trustworthy, and a half-correct one (sandbox failures
silently green-checked) is worse than no indicator at all.
