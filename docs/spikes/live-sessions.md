# Spike: messaging live Claude Code and grok sessions (#9)

Date: 2026-10-27. Versions: Claude Code 2.1.287, grok 1.0.52.

## Short answer

| Tool | Can lasso put a message into a running session? | Recommendation |
|---|---|---|
| Claude Code | **Yes, indirectly.** Claude Code has built-in cross-session messaging between sessions on one machine. It is only reachable from another Claude Code session, not from an outside program. lasso can run a small Claude session of its own and have it pass the message on. | #41: relay through a lasso-owned Claude session, and read busy/idle from `~/.claude/sessions/<pid>.json`. |
| grok | **Only when the user turns on leader mode** (`[cli] use_leader = true`, off by default). Then the session lives in a shared leader process, and lasso can join it as a second ACP client and queue a prompt. A grok TUI that isn't in leader mode has no documented endpoint. | #42: send through the leader when one owns the session; otherwise keep the current rule. |
| Fallback (both) | "Send on next idle" is **not** safe for a session that is still open in a TUI, even when it is idle. Resuming it headless makes a second writer, and the conversation forks. | Without the paths above, queue the message until the session's **process exits**, not until it goes idle. |

No tmux keystroke injection was tried or is proposed.

## Claude Code

### What was tried

| Command / probe | Result |
|---|---|
| `claude --help` | No "send to a session" flag or subcommand. Relevant: `--bg`, `attach`, `logs`, `agents`, `--remote-control`, `--input-format stream-json`. `--bg --resume <id>` on a running session "starts a copy and says so", which confirms that a second writer forks. |
| `claude send/message/msg/peers/inbox --help` | Not commands. Each falls through to the top-level help. |
| `claude remote-control --help` | Remote Control relays a session through claude.ai/code or the mobile app. It only covers sessions started with Remote Control, and the relay is Anthropic's cloud. There is no local API. Not useful to lasso. |
| `claude agents --json` | **Useful, read-only.** Prints a JSON array of active sessions on the machine, both interactive and background: `pid`, `sessionId`, `cwd`, `name`, `kind`, `status` (`busy` / `idle`), and `state` for background ones (`blocked` = waiting on the user). Needs no TTY. |
| `~/.claude/sessions/<pid>.json` | The file behind that list. One per running session: `status` (`idle`/`busy`/`shell`), `statusUpdatedAt`, `sessionId`, `cwd`, and `messagingSocketPath` (`/run/user/<uid>/cc-socks/<pid>.sock`). A mode-600 `<pid>.<hash>.key` sits beside it. I did not read the key files. |
| `ss -xlp` | Each interactive session listens on its `cc-socks/<pid>.sock`. Background sessions also have sockets under `/tmp/cc-daemon-<uid>/` (pty, control). |
| Agent SDK / `claude -p --input-format stream-json` | Works for sessions lasso starts itself and keeps stdin open to. It can't attach to a session someone else started in a terminal. |
| Throwaway session: `claude -p --input-format stream-json --output-format stream-json --name lasso-spike-probe --strict-mcp-config --tools ""`, stdin held open by a fifo, in a temp dir | It registered itself as a peer: `~/.claude/sessions/<pid>.json` with `"entrypoint":"sdk-cli"` and its own messaging socket. |
| From this (spike) Claude session: `SendMessage(to: "lasso-spike-probe", message: "…reply with just the word PONG.")` | **Delivered.** The idle probe started a turn by itself and answered `PONG`. No input on its stdin. |

### What arrives, and what the user sees

The message lands in the receiver's transcript as a `type:"user"`, `isMeta:true` entry with `"origin":{"kind":"peer", "verifiedPeerPid":…, "name":"<sender's session name>", "fromMode":"…"}` and `"turnOrigin":"peer"`. Its text reads:

```
Another Claude session sent a message:
<cross-session-message from="uds:/run/user/1000/cc-socks/<pid>.sock" from-name="dashboard-bf" from-mode="prompting">
…the text…
</cross-session-message>

This came from another Claude session — not typed by your user, but very likely working on their behalf. …
```

- **Idle session:** the message starts a turn right away (seen above).
- **Busy session:** the message is queued and taken in at the session's next tool round. This is documented in the `SendMessage` tool; I didn't try it against a busy session.
- **In the TUI:** the user sees it as a peer message in the conversation, with its first line as a preview until they expand it (per the tool's own docs; I didn't watch a TUI render it).
- **Permission modes:** a receiver whose permission mode differs from the sender's holds the message for its user to approve. The sender gets a `[Cross-session delivery notice]` when that happens. For lasso this is a feature, not a problem: a message the user didn't expect waits for them.
- **Trust:** the receiving model treats the message as a teammate's request, not as the user typing. It can't approve a pending permission prompt with it. So this path **sends messages; it does not answer permission prompts**. Prompts stay as today: they're inferred from the transcript, and the user answers them in the session.

### Can lasso talk to the socket directly?

Not sensibly. The protocol is private and versioned (`"peerProtocol":1`, `peerFeatures`). The receiver records `verifiedPeerPid` and `verifiedPeerProcStart` for the sender, which suggests it checks that the sender is a registered Claude Code process, and the per-session `.key` files look like part of that. Reverse-engineering it would break on any Claude Code update, and it would pass lasso off as a Claude session. **Rejected.**

### Relaying through a lasso-owned Claude session

lasso starts one headless Claude session of its own, the relay, with:

```
claude -p --input-format stream-json --output-format stream-json \
  --name lasso --strict-mcp-config --tools SendMessage …
```

To send, lasso writes one stream-json user message telling the relay to call `SendMessage` with the target session's name and the user's text, unchanged. Then it watches the relay's stream for the tool result: `queued`, `held for approval`, or `refused`.

- **Pros:** supported mechanism; works on open TUI sessions, idle or busy; the user sees it arrive in their own session; held-for-approval keeps the user in charge.
- **Cons:**
  - A model sits in the path. lasso must check that the `SendMessage` input it sees in the stream is the user's text, byte for byte.
  - Each message costs a small relay turn.
  - The receiver sees it as "another Claude session", not as the user.
  - Targets are addressed by session **name** (with a `[ref]` when two share one), not by UUID. lasso maps `sessionId` to `name` with `~/.claude/sessions/*.json`.
  - lasso already skips its own sessions, so the relay won't show up in lasso's lists.

### Hooks

A lasso-installed `Stop` hook could block the end of a turn and feed a queued message back in as the reason. A `UserPromptSubmit` hook could add it as context. Both would work for busy sessions only; an idle session runs no hooks until something else wakes it. ADR 0001 §4 says lasso installs no hooks into the bots, and this would mean editing the user's `~/.claude/settings.json`. **Rejected:** a worse version of the relay that breaks a stated rule.

### Idle detection for the fallback

`~/.claude/sessions/<pid>.json` (or `claude agents --json`) gives `status: idle|busy` straight from Claude Code. That is better than today's guess from how recently the transcript changed, and lasso should use it for liveness whatever else happens. But "idle" here means the TUI is open and waiting for input. Resuming it with `claude -p --resume` still forks the conversation. So without the relay, the safe fallback is: queue until the pid is gone (the file disappears), then resume headless.

## grok

### What was tried

| Command / probe | Result |
|---|---|
| `grok --help` | Headless only (`-p`, `--prompt-file`, `--resume`, `--allow/--deny`), which is what the adapter already does. No "send into a running session" flag. |
| `grok dashboard` + `~/.grok/docs/user-guide/23-dashboard.md` | The dashboard lists and replies to sessions **in its own process**. Sessions owned by other grok processes appear as "Inactive", roster-only. It can't deliver into another TUI, and it's a TUI, not an API. |
| `grok leader list`, `grok leader info` | `No leader candidates found` / `no reachable leader`. Leader mode is off on this machine. `~/.grok/config.toml` has no `[cli] use_leader`, and `02-authentication.md` says it is off by default. |
| `grok agent --help`, `15-agent-mode.md` | `grok agent stdio` / `serve` speak **ACP** (JSON-RPC: `initialize`, `session/new`, `session/load`, `session/prompt`, `session/update`, permission requests). `grok agent --leader` joins a shared leader over `~/.grok/leader.sock`. The leader is also what TUIs use when `use_leader` is on ("leaders auto-spawned from interactive clients (TUI/IDE)"). `25-status-line.md` describes "another client's turn" showing in the TUI as `remote-turn-<id>` on the daemon route. |
| `ss -xlp`; `~/.grok/active_sessions.json` = `[]` | No grok TUI was running, so I couldn't list a live TUI's sockets. The docs describe no endpoint for a TUI outside leader mode: its agent runs in-process. |
| Throwaway leader: `grok agent leader --leader-socket /tmp/lassospike-leader.sock --relay-on-demand --no-auto-update --no-exit-on-disconnect`, then two ACP clients (`grok agent --leader --leader-socket … stdio`). Client A: `session/new` + `session/prompt`. Client B: `session/load` of A's session + `session/prompt "Reply with just PONG."` | **B's prompt reached the shared session, and A saw it.** A got `_x.ai/queue/changed` with B's text queued at position 0, then `runningPromptId`/`runningText`, then `hook_run_started`, `turn_completed`, `_x.ai/session/prompt_complete`. The model turn itself failed with `402 Grok Build usage balance exhausted`, so I couldn't see a full answer or a permission prompt go through both clients. The routing and the queue are confirmed. The throwaway session was deleted with `grok sessions delete`. |
| `~/.grok/docs/user-guide/10-hooks.md` | grok has `Stop` / `UserPromptSubmit` hooks under `~/.grok/hooks/`. Rejected for the same reason as Claude's. |

I didn't read `~/.grok/auth.json`.

### What works

With leader mode on, a TUI's session runs in the leader, and lasso can:

1. Join with `grok agent --leader stdio` (or speak ACP on the socket with `acp-go-sdk`).
2. `session/load` the session.
3. `session/prompt` the user's text.

grok queues the prompt itself if a turn is running, so lasso needn't wait for idle. The user's TUI sees the queued prompt and the turn that follows. Permission requests in the shared session probably go to every attached client as ACP permission requests. If they do, lasso could answer them too, replacing the "resume with `--allow`/`--deny`" workaround for live sessions. That is **not verified** because of the 402.

Without leader mode, nothing works. lasso shouldn't switch it on for the user: it changes how all their grok sessions run.

### Idle detection for the fallback

- `active_sessions.json` says a session is open in a TUI (with a pid), not whether it's busy.
- The session files show an unfinished turn, which the adapter already reads.
- Leader clients get `queue/changed` and `prompt_complete`, which is exact, but only in leader mode.

As with Claude, an open-but-idle non-leader TUI can't be resumed safely. So the fallback is: queue until the pid leaves `active_sessions.json` (or dies), then resume headless as today.

## Recommendation

1. **Claude Code:** build the relay and switch liveness to `~/.claude/sessions/*.json`. Follow-up ticket: #41.
2. **grok:** when a leader owns the session, send (and later answer prompts) as an ACP client of the leader. Otherwise keep the session read-only and say so in the UI. Follow-up ticket: #42.
3. **Both:** for sessions neither path reaches, "deliver on next idle" becomes "deliver when the session's process exits". The UI should say "queued until you close this session" rather than "can't send". This is part of each ticket.

## What building it found (#41, #42, #51)

Both paths are built: the Claude Code relay (#41, PR #49) and the grok leader client (#42, PR #48). Building them corrected or added to the spike in these ways.

### grok's leader socket isn't plain ACP

The spike assumed lasso could speak ACP on `~/.grok/leader.sock`. It can't, not directly.

- Each message is a frame: a 4-byte big-endian length, then a JSON envelope.
- The client registers first (`{"type":"register",…}`) and gets back `{"type":"registered","leader_protocol_version":1,…}`.
- After that, every ACP message travels as `{"type":"acp","payload":"<the JSON-RPC message, as a string>"}`.

lasso speaks this framing itself. It doesn't use `grok agent --leader stdio` as a bridge, because that command starts a new leader when none answers, and lasso must never start one. If the leader reports a protocol version other than 1, lasso says so once and carries on as if there were no leader.

### Loading a shared grok session must send `yoloMode: false`

If `session/load` leaves out `_meta.yoloMode`, the shared session switches to the loading client's default permission mode. For a user whose grok config says always-approve, loading a session from lasso would have turned an ask-mode session into an always-approve one. lasso always sends `"_meta": {"yoloMode": false}`. That was seen to keep an ask-mode session in ask mode, and to leave an always-approve session alone. A test pins it.

### lasso's relay checks every call before it runs

The spike planned to read the relay's `SendMessage` call out of its output stream and compare it with the user's text. By then the call has already run. Instead, when lasso starts its relay it registers a PreToolUse hook over Claude Code's SDK control protocol. The hook lives only in the relay's own process; nothing is installed into the user's Claude Code.

lasso allows a call only if it is the one `SendMessage` it asked for, to that exact session name, with that exact text. Anything else, such as changed text, another target, a paraphrase or another tool, is denied before it runs, and the send fails with an error. After a send, lasso also checks the receiver's transcript for the matching peer entry.

### Answering grok prompts through the leader is still unverified

The grok account used for testing returns `402 Grok Build usage balance exhausted`, so no model turn ran to completion through the leader and no permission prompt was ever raised. lasso's code for answering a leader's `session/request_permission` (with the request's own allow-once or reject-once option, never "always") follows the ACP spec and is tested only against a fake leader. It is also still unknown whether the leader sends a shared session's permission requests to every attached client, or only to the client whose prompt is running.

### Held messages (#51)

When neither path reaches an open session, lasso holds the message until the session closes, as recommended above.

- `Send` now says what happened to a message: sent, held, or held for approval (Claude Code holds a relayed message when the receiving session runs in a different permission mode). lasso says "sent" only for a delivered message. For a held one it says the message is waiting until that session closes.
- Held messages are kept in `lasso.db` (table `held`) with the adapter, session, text and time. Each is deleted once it goes out. After a restart the adapters reload them, so a restart no longer loses them.
- The audit log gets a `hold_message` line when a message is held and a `deliver_message` line when it goes out (or fails to), with actor `lasso`.
- On shutdown the daemon closes the Claude Code relay session and any open grok leader connections.
