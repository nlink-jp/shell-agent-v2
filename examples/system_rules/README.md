# System Rules — examples

Copy-paste templates for `<dataDir>/system_rules.md`. Each file in
this directory is a self-contained set of standing instructions
addressing one recurring agent-bias or workflow problem.

To use one:

1. Open the file, copy the entire content
2. In the running app, open **Settings → System Rules**
3. Paste, hit **Save**
4. Start a new chat (existing sessions inherit in-context
   conditioning from prior turns and will not pick up the change
   cleanly — by design, see [ADR-0012 §3.4](../../docs/en/adr/0012-system-rules.md))

You can also combine rules from multiple examples in one file —
System Rules is plain Markdown with no schema.

See [`docs/en/reference/system-rules.md`](../../docs/en/reference/system-rules.md)
for the design and full semantics.

---

## Available examples

| File | When to use |
|------|-------------|
| [`activity-log-audit.md`](activity-log-audit.md) | Auditing terminal / user activity logs. Counters the LLM's strong prior to invent attacks ("spy", "compromised", "cyberattack") from routine sysadmin / dev activity. Forces evidence-based reporting with explicit benign alternatives. |

---

## Contributing new examples

A good example:

1. Solves a **specific, repeated** bias or workflow problem (not a
   "general purpose preferences" template — that's what Global
   Memory is for).
2. Names the bias it counters in a short header.
3. Uses **hard rules with explicit forbidden words / phrases**
   rather than vague preferences. LLMs honour hard rules far
   better than soft suggestions.
4. Includes a **calibration ladder** (Observed / Possible / Likely)
   when the domain involves uncertain inference.
5. Is short enough to copy-paste in one go — aim for under 100
   lines.

Open a PR adding the new file plus a row in the table above.
