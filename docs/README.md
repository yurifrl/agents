# Documentation — the source of truth

**This directory is the single source of truth for the `agents` project.**

Read this before writing code, reviewing changes, or answering architecture
questions. Code, manifests, and conversation are *evidence*; when they disagree
with these docs, either the code is wrong (fix it) or the docs are stale (update
them). They must never be allowed to drift silently.

Division of knowledge in this repo:

- **`docs/` knows things.** Architecture, decisions, conventions, current state.
  This is where truth lives and the only place it lives.
- **`.claude/agents/` know how to *do* things, not what.** They hold no project
  knowledge of their own; they read `docs/` to learn the truth, then act
  (audit adherence, update docs). Swap the docs and their behavior changes.
- **`.claude/skills/` know the *workflow*.** When to act, which agent to fire,
  in what order — procedure, not facts.

If you find yourself "remembering" a project fact that isn't written here,
that's a bug: write it down or you're operating on drift.

## Map

- [`architecture.md`](./architecture.md) — what the system is, package layout,
  and the decisions (with rationale) that constrain it.
- [`conventions.md`](./conventions.md) — the rules every change must satisfy,
  and the anti-patterns that count as cutting corners.
- [`status.md`](./status.md) — what is actually built vs. pending, right now.
