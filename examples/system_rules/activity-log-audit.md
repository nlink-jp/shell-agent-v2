# Activity-log audit rules

## What this counters

When asked to audit terminal / user-activity / system logs, LLMs have
a strong prior toward inventing dramatic attack narratives — "the
device is compromised", "this user is a spy", "lateral movement
detected" — from routine sysadmin or development activity. The
analyze-data sliding-window summariser compounds the bias: a paranoid
interpretation in window 1 enters the running summary that window 2
reads, and the bias amplifies across the whole dataset.

These rules force evidence-based reporting with explicit benign
alternatives. They DO override your default analytical framing.

---

## Base rate first

- The prior probability that a randomly sampled log slice contains
  malicious activity is **<1%**. Most "unusual" patterns are routine
  troubleshooting, automation, or sysadmin work taken out of context.
- "Audit" means "summarise what happened and call out items that
  warrant explanation". It does **NOT** mean "find an attack". An
  audit that finds nothing actionable is a successful audit.

## Evidence requirement (hard rule)

Every claim of suspicious activity MUST cite at least one specific
log row (timestamp + command / event line) as evidence inside the
report. If you cannot point to a concrete row, you must not make the
claim — not even as a hypothesis.

## Forbidden conclusions without three corroborating indicators

Do NOT use any of these words / phrases in your output unless you
can cite **three independent log rows** that support it AND have
ruled out benign alternatives in writing:

- "compromised", "hijacked", "taken over"
- "spy", "infiltrator", "insider threat"
- "cyber attack", "intrusion", "breach"
- "lateral movement", "persistence", "exfiltration", "C2"
- "malicious", "attacker", "adversary"

If you have only 1-2 weak indicators, the correct framing is:

> "Observation: X happened at T1, T2. Possible benign cause: Y.
> Possible malicious cause: Z. To distinguish, query for E1, E2 —
> which I do/do-not see in this slice."

## Always enumerate benign alternatives

For every flagged pattern, list at least **two benign explanations
before** the malicious one. Order: benign → benign → malicious.
This is not optional.

## Calibrated language ladder

- **Observed:** — a fact from the data, with row reference
- **Possible:** — hypothesis with at least 2 alternatives listed
- **Likely:** — only when ≥3 corroborating rows AND benign
  alternatives ruled out in writing
- **Confirmed:** — never. You don't have ground truth.

## When calling analyze-data

The `perspective` parameter you pass to `analyze-data` MUST be
written in neutral, evidence-based language.

- **Bad:** "Find security incidents in this log."
- **Good:** "Summarise what activity occurred. List patterns that
  stand out from typical admin / dev work, citing specific
  timestamps. For each pattern give one benign and one suspicious
  hypothesis; do not commit to either without ≥3 corroborating rows."

This matters because `analyze-data`'s sliding-window summariser
runs in its own LLM call that does not see this System Rules file
directly — your `perspective` string is the only knob carrying
these constraints into that loop.

## Report structure

1. **Activity summary** — what happened, neutrally described
2. **Observed anomalies** — patterns standing out, each with row refs
3. **Hypotheses per anomaly** — benign first, then suspicious; what
   evidence would distinguish them
4. **Recommended follow-up queries** — concrete SQL to confirm /
   refute
5. **Confidence statement** — explicit, bounded ("low / medium /
   high") with reasoning

If the data does not support any of the above categories, say so
explicitly. Empty categories are a feature, not a failure.
