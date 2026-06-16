---
name: project-workflow
description: The development workflow for the agents repo. Use at the start of any work session in this repo and to know when to fire the governance agents. Explains that docs/ is the source of truth, and triggers the architecture-guardian and docs-keeper agents every 5-10 inputs or changes.
---

# Project Workflow

This skill knows the **workflow**, not the project. The project's truth lives in
`docs/` — read it; don't carry facts in your head. The agents
(`.claude/agents/`) know *how* to audit and how to update docs; this skill knows
*when* to fire them and in what order.

## The loop

1. **Orient from truth.** Before non-trivial work, read `docs/README.md` and the
   relevant doc (`architecture.md`, `conventions.md`, `status.md`).
2. **Do the work** within the conventions. Declarative over imperative; secrets
   via ESO; reproducible from git + 1Password (the litmus test).
3. **Govern on a cadence** (below).
4. **Keep docs true** as part of finishing, not as an afterthought.

## Cadence: when to fire which agent

Maintain a tally of **inputs or changes** since the last governance pass in
`~/.local/state/agents/governance.md` (create it if missing; format below).
Count each user input and each batch of file changes.

- When the tally reaches **5**, you *may* run a governance pass; by **10** you
  *must*. Also always run one before declaring a piece of work done, and after
  any architectural change.
- A governance pass is, in order:
  1. Fire **`architecture-guardian`** — audits the recent changes against
     `docs/` and reports adherence / corner-cutting. Act on its findings.
  2. Fire **`docs-keeper`** — reconciles `docs/` with the new reality and records
     decisions and debt.
- After the pass, reset the tally to 0 and note the pass in the state file.

Fire the guardian *before* the docs-keeper: confirm what's true, then write it
down.

## State file format (`~/.local/state/agents/governance.md`)

```
last_pass: <date or commit>
tally_since_pass: <n>
notes: <optional, e.g. "guardian flagged hand-applied secret; logged as debt">
```

This file is operational state, not project truth — it lives **outside the repo**
(`~/.local/state/agents/`), never in `docs/` and never committed.

## Reminders

- If you "know" a project fact that isn't in `docs/`, that's drift: have the
  docs-keeper write it down.
- Don't skip the pass because the change "obviously works." Working is not the
  bar; adherent and reproducible is.
