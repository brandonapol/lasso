# lasso

One quiet window over your repos and your bots. A three-pane terminal app (and
an optional browser view on `localhost:7337`): an assistant that talks to you in
plain words, tabs for issues, Actions runs, work in progress and daily
summaries, and a focus pane for whatever you select.

- Backend design: [docs/adr/0001-backend.md](docs/adr/0001-backend.md)
- UI design: [three-pane canvas](https://claude.ai/artifact/AQwVdUiiANrybqk9bfxTT1)
- Work: GitHub issues #1–#14

## Rules

`~/.config/lasso/rules.toml` says what lasso may do for you without asking.
With no rules it asks every time. The assistant goes through the same rules:
when it wants to message a bot or answer one of its prompts and no rule
allows it, it asks you in chat, and your answer goes through as yours.

A fixed always-ask list (login and auth code, billing, secrets, merging to
the main branch, deleting branches, force-pushing, spending money, changes over
500 lines) wins over every rule. lasso reads those facts from the bot's own
records of the call it wants to make, not from what the bot says about it.

An example. Uncomment it to let the assistant message any of your bots, up to
20 times a day:

```toml
# Let lasso pass your messages on to any bot session: saddle tasks
# (saddle-<task>), Claude Code (claude-<id>) and grok (grok-<id>).
# [[rules]]
# id = "lasso-may-message-my-bots"
# who = "*"                    # or "saddle-*" for saddle tasks only
# action = "send_message"
# per_day = 20
# repos = ["you/*"]            # optional: only sessions working in these repos
```
