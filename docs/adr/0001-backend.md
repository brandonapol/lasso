# ADR 0001: lasso's backend, a quiet local daemon behind two thin clients

- Status: Proposed
- Date: 2026-10-27
- Tickets: #1–#14
- Design: [three-pane canvas](https://claude.ai/artifact/AQwVdUiiANrybqk9bfxTT1) (top row; the screens below it are earlier exploration)

## Context

lasso is one window for everything Brandon's bots and repos are doing:

- **Left pane:** an agent that talks to him in plain words and passes his instructions to the bots.
- **Middle pane:** tabs for Issues, Actions runs, In progress, and Summaries. Summaries is a Sonnet-written account of the last 24 hours in each hot repo.
- **Right pane:** a focus pane for whatever is selected in the middle.

It runs in the terminal first. A browser view of the same thing is optional.

The bots are:

- **saddle**, which runs several coding agents per repo and already serves its state over a loopback MCP endpoint (`saddle remote`, see saddle's `docs/remote-control.md`).
- **Claude Code sessions**, whose transcripts live in `~/.claude/projects/<cwd>/<session>.jsonl`.
- **grok bots** (Grok Build CLI). Its sessions live under `~/.grok/sessions/<cwd>/<id>/`, and it has a headless mode (`--resume`, `--output-format streaming-json`).

The grok bots in particular produce hundreds of messages an hour.

Most of this is a dashboard: read GitHub and a few local files, then show them. The hard parts aren't technical. They're about behaviour:

1. **Saddle's lessons.** Saddle is useful, but in day-to-day use it is loud. It pings, raises needs-you prompts in agent jargon, and pulls attention to itself. It also paints itself into corners. It keeps a lot of its own mutable state (the merge train, the stack, the integration branch, claims), that state drifts from GitHub, and getting back takes recovery commands (`restack`, `requeue`, `sentinel ack`). lasso must not repeat either.
2. **Acting for the user without asking every time.** Letting a bot file a ticket or accept a CI retry must be possible without a prompt. It must be bounded, logged, and possible to undo.

## Decision

### 1. One daemon, two thin clients

`lasso serve` is a single Go process (`lassod` in prose). It owns polling, state, the model calls and the bot connections.

- The TUI (`lasso`, Bubble Tea) and the web view are clients of its local HTTP API. Neither one holds state of its own.
- If no daemon is running, `lasso` starts one in the background.
- Go matches saddle (Bubble Tea, lipgloss, cobra, `modernc.org/sqlite`), so ideas and small pieces of code carry over.
- saddle's packages are `internal/`, so lasso **copies** the few it borrows, with a header naming where each came from. It doesn't import them. Saddle's runtime state is reached only through `saddle remote` (section 4).

The API listens on `127.0.0.1:7337`. It uses JSON endpoints plus one Server-Sent Events stream, `GET /events`, which pushes view changes to the clients. The checks are the ones saddle's remote server already uses:

- loopback only;
- refuse requests that carry an `Origin` header (except from the web view's own origin);
- the `Host` must be loopback;
- a bearer token read from `~/.config/lasso/token` (mode 0600), which the TUI reads itself and the web view receives once through `lasso open`.

### 2. lasso owns no work state

The source of truth is GitHub, saddle, and the bots' own session files. lasso never derives a workflow state that it then has to keep in sync.

**What lasso stores** is only what can't be read again from somewhere else:

| Kept | Where | Why |
|---|---|---|
| Config: hot repos, interrupt budget, quiet hours, model caps | `~/.config/lasso/config.toml` | the user edits it |
| Rules (standing permissions) | `~/.config/lasso/rules.toml` | the user edits and reviews it, and keeps it in git if they like |
| Audit log: every action lasso or a rule took for the user | `~/.local/state/lasso/audit.jsonl` | for undo and for "who did this" |
| Answers to decisions, and read cursors ("last seen") | SQLite, `~/.local/state/lasso/lasso.db` | so the brief knows what's new |
| Caches: GitHub responses, digests, summaries | the same SQLite file, with TTLs | for speed only |

Deleting `lasso.db` loses read cursors and caches, nothing else. A test enforces this: it deletes the database, restarts, and checks that rules and the audit log are intact and that the views come back.

**What lasso never does on its own:**

- push, rebase, merge, land, or delete branches;
- run multi-step git workflows.

When work needs doing, lasso asks a bot to do it. Every outward action is **one** call through an adapter (comment, label, rerun a run, send a message, answer a prompt). Each is idempotent where the target allows, and each is written to the audit log *before* it is sent. If an action fails, lasso shows the failure and stops. It doesn't retry into a workaround.

That is the guard against painting into corners: lasso has no state that can get stuck, so there's nothing to recover.

### 3. GitHub source

Each hot repo is polled with `gh api` (GraphQL for issues, PRs and milestones; REST for Actions runs). It goes through one runner, the same shape as saddle's `internal/forge` exec layer.

**Polling cadence:**

| What | How often |
|---|---|
| Actions runs | 60 s while any run is in progress, otherwise 5 min |
| Issues and PRs | 2 min |
| Milestones | 15 min |

REST calls use conditional requests (ETag), and polling backs off when the rate limit falls below 20%.

**An issue's due date** is the first of these that exists:

1. a Projects v2 date field named `Due` or `Target date`;
2. its milestone's `due_on`;
3. a `Due: YYYY-MM-DD` line in the body.

The focus pane shows which source it came from.

### 4. Bot adapters

There is one Go interface. Each adapter reads its bot's own files or API, so there are no hooks installed into the bots.

```go
type Adapter interface {
    Kind() string                                   // "saddle", "claude", "grok"
    Sessions(ctx) ([]Session, error)                // what's running, idle, waiting
    Events(ctx, since Cursor) ([]Event, Cursor, error) // new messages / prompts / state changes
    Send(ctx, sessionID, text string) error         // a message from the user (via the agent pane)
    Answer(ctx, promptID string, choice Choice) error // reply to a permission/decision prompt
}
```

| Adapter | Reads | Writes |
|---|---|---|
| saddle | `saddle remote` MCP over loopback: snapshot, needs-you queue | `saddle remote`'s confirmed admin actions. lasso holds a read token by default; the user mints an admin token to enable writes |
| Claude Code | transcript JSONL under `~/.claude/projects` (tailed with fsnotify) | idle sessions: `claude -p --resume <id>`; live sessions: spike (#9) |
| grok | `~/.grok/sessions/**` and `grok sessions` | idle sessions: `grok --resume <id> --prompt-file … --output-format streaming-json`; live sessions and in-session permission prompts: spike (#9) |

An adapter that can't write to a session marks it read-only. The UI then says "can't send to this one while it's running" instead of failing silently.

### 5. Attention: three levels, a budget, and silence by default

Every event becomes an *item* at exactly one level:

| Level | Meaning | Where it shows |
|---|---|---|
| `log` | happened, doesn't matter | raw log and history only |
| `brief` | worth knowing | the left pane's "since you left" message, the Summaries tab, the focus pane |
| `ask` | needs a human decision a rule doesn't cover | In progress tab, counted against the budget |

**How an item gets its level:**

- **Level rules first.** These are deterministic Go code, not a model. Examples: a failing Actions run on a default branch is `brief`; a permission prompt that no standing rule covers is `ask`.
- **Then Haiku** sorts whatever the rules left unclassified, choosing between `log` and `brief` only. It can never raise anything to `ask`.

**The interrupt budget** defaults to 2 `ask`s a day.

- Further `ask`s wait, and are presented the next day or when the user opens In progress.
- **Line-jumpers** may skip the budget. They are a fixed, short list: the default branch red in a hot repo for more than 30 minutes, something due within 24 hours with no progress, or a bot blocked on the user for more than 2 hours.
- Desktop notifications are **off by default**. When turned on, they fire for line-jumpers only. There's no terminal bell, and lasso never takes focus.

**Every `ask` uses the decision-card shape:**

- what the bot wants;
- why it's asking (which rule it falls outside);
- the plain version;
- what happens if yes and if no;
- lasso's recommendation.

A glossary maps bot jargon to plain words ("stale base" → "the branch it started from has moved on"). The writer is told to avoid jargon, and a lint test checks the decision-card prompts against the glossary.

### 6. Rules (standing permissions)

**Format.** Rules live in TOML. Each one names who (`grok-*`), what action kind (`file_issue`, `ci_retry`, `fix_small`, `answer_prompt`), conditions (labels, path globs, line limits, per-day caps), and an expiry if there is one.

**Matching** is deterministic Go code. The LLM never decides whether a rule allows something. An `always_ask` list (login, `billing/`, secrets, merging to main, spending money, changes over 500 lines) overrides every rule.

**Creating rules.** The agent pane may *draft* a rule from plain words ("let the grok bots file flaky-test tickets"). The draft is shown as structured TOML and saved only after the user confirms.

**Suggesting rules.** After the same answer has been given 3 times in 7 days, lasso offers to make it a rule. The offer is a `brief` item, not an `ask`.

**Recording.** Every rule-allowed action goes to the audit log with the rule's id. Undo means the inverse action where one exists (close the filed issue, remove the label), or "can't undo, here's what happened" where it doesn't.

### 7. Models

| Use | Model | Cap |
|---|---|---|
| Event digest ("412 messages → 3 points"), level sorting | `claude-haiku-4-5-20251001` | default $1/day |
| Summaries tab (24h per hot repo), decision cards, agent pane | `claude-sonnet-5-5` | default $5/day |

- **Inputs are compact structured deltas,** not raw pane text, the way saddle's narrator does it. lasso borrows narrator's batching, salient-flush and daily-cap design.
- **All bot and GitHub text is untrusted.** It is fenced with a random nonce before going into any prompt, and clipped.
- **Past a cap,** the feature falls back to local text ("12 new messages from grok-scout") and says it's over the cap.
- **Summaries** are generated at most once an hour per repo, or when the user asks.
- **The agent pane's tools** are calls to the daemon API: list and focus items, send to a bot, answer a prompt, draft a rule. Every write goes through the same rules check and audit log as anything else. The agent cannot widen its own permissions.

## Consequences

- **Good:** lasso can't break a repo or get stuck. Reinstalling or deleting its database is safe. Each behaviour (budget, rules, levels) is a testable Go function, not a prompt.
- **Good:** there's one place to look, and there's no new state for the bots to keep in sync with.
- **Cost:** writing into *live* Claude Code and grok sessions isn't solved by their CLIs. Until the spikes land, lasso can only message idle sessions, plus saddle through `saddle remote`.
- **Cost:** copying saddle code means fixes don't flow both ways. We accept that, because the copied pieces are small (exec runner, narrator batching, remote's request checks).
- **Not doing:** autopilot, planning, task graphs, landing, or any git operations. Those stay with saddle or the bots.

## Open questions

- **Live sessions:** is there a supported way to message a running Claude Code session? (Claude Code's local session messaging is a candidate.) What about a running grok session (`grok leader`)? Both are covered by #9.
- **Hot repos:** should the list be explicit in config only, or seeded from repos with activity in the last 7 days? The proposal is explicit config, with lasso suggesting additions as a `brief` item.
