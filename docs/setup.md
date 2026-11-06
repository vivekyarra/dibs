# Setup

From zero to a running Dibs room in about 15 minutes. Steps 1–4 give you the full workflow. Steps 5–6 make it run on its own.

## 1. Install

```sh
go install github.com/vivekyarra/dibs/cmd/dibs@latest   # or download a release binary
dibs demo                                                 # optional: see the whole story offline first
```

The agents call `dibs` from their working directory, so install it on the machine where Switch Console runs your agents.

## 2. Switch

1. Install [Switch Console](https://docs.switchagents.ai/switch-rooms/getting-started/install-switch-console.md) and add a local server (Console runs it in Docker).
2. Connect your chat app: [Discord](https://docs.switchagents.ai/switch-rooms/deploy/messaging-apps/discord.md) or [Slack](https://docs.switchagents.ai/switch-rooms/deploy/messaging-apps/slack.md).
3. Link each person's chat account to their Switch user, so the agents recognise who is talking.
4. Under **agent providers**, sign in to Claude Code and Codex.
5. Under **Settings → Connections**, connect GitHub and grant it the repository Dibs will work on.

## 3. Your repository

1. From your repo root, run `dibs scaffold --senior <github-login> --junior <github-login>`. It detects the repo from your git remote, infers one skill per source folder (plus `testing`), and writes `.dibs/`. Review it (a complete example is [`examples/demo-shop/.dibs`](../examples/demo-shop/.dibs)):
   - `team.yaml`: the repo, the team room name, seniors and juniors (GitHub logins, chat names, each junior's starting skill levels 0–4).
   - `skills.yaml`: the skills your codebase teaches, with path globs and keywords.
   - `dibs.yaml`: the learning budget and timers (the defaults are a good start).
2. Add the guard and make it a required check on your default branch:

   ```yaml
   # .github/workflows/dibs-guard.yml
   name: dibs-guard
   on: { pull_request: { types: [opened, edited, synchronize, reopened] } }
   permissions: { issues: read, pull-requests: read }
   jobs:
     guard:
       runs-on: ubuntu-latest
       steps:
         - uses: actions/checkout@v5
         - uses: vivekyarra/dibs@main
   ```
3. Run `dibs init` once (creates the labels and each junior's skill map), then `dibs doctor` to check everything.

## 4. Create the rooms

In Switch Console, open **Templates → Import template**, paste [`switch/dibs.template.yaml`](../switch/dibs.template.yaml), then **Use**. Fill in the team name, the junior's chat username and handle, and the repo, then press **Create all**.

This creates:
- agents `<team>-dibs` (Claude Code), `<team>-builder` (Claude Code) and `<team>-reviewer` (Codex);
- the group `dibs-<team>` with `<team>-team` and `<junior>-practice`, linked by `practice`;
- the Dibs Protocol as room instructions and a room document;
- aliases `@dibs`, `@builder` and `@reviewer`.

To make the senior's approval a native Switch prompt, copy [`agents/dibs.claude-settings.json`](../agents/dibs.claude-settings.json) to `.claude/settings.json` in the dibs agent's directory. `dibs approve` then shows **Allow once / Decline** to the agent's owner, who is the senior who ran the template.

Say `@dibs sweep` in the team room. The first card appears.

## 5. Autopilot

`dibs autopilot` runs Dibs with nobody typing. Every interval (default 5 minutes) it syncs GitHub, triages new issues with its suggestions, scores, applies deadline and stuck rules, and posts the results into the rooms. Every Monday at 09:00 UTC it posts the weekly ramp report. Judgment work, like walkthroughs and room documents, goes to `@dibs`.

```sh
dibs intake register --server https://<your-switch-server> --registration-key <REG_KEY>   # once, see below
dibs autopilot                    # posts into Switch as dibs-intake
dibs autopilot --once --dry-run   # one cycle, print instead of post
```

Run it as a service: a systemd unit, a container, or a scheduled task.

### Intake: GitHub events into the room

`dibs-intake` is a small custom Switch agent built on the Switch agent protocol. It turns GitHub webhooks into one batched message to `@dibs`, so the room reacts the moment an issue opens or a PR merges.

### Register the agent

1. In the Switch Gateway, open **API Keys** and create a **registration key**.
2. Register:

   ```sh
   dibs intake register --server https://<your-switch-server> --registration-key <REG_KEY>
   # optional: --name dibs-intake --out ~/.dibs/intake.json --overwrite
   ```

   You can also set the key with `SWITCH_REGISTRATION_KEY` and the server with `SWITCH_SERVER`, which keeps the key
   out of your shell history.

   This calls `POST /agents` with `connector_type: "dibs-intake"` and saves `{server, agent_id, api_key, name}` to
   `~/.dibs/intake.json` with mode `0600`. Switch shows the agent API key **only once**. The command prints a masked
   form (`agen…cdef`) and never the full key.

### Invite it to the team room

A person must add the agent. In the team room, type:

```
!invite-agent @dibs-intake
```

Until you do this, `list_rooms` will not return the room and `serve` logs `could not resolve room yet`. It keeps
running and resolves the room again when it sends its first message.

Quick check:

```sh
dibs intake send --room team --text "dibs-intake is connected"
```

### Run the webhook receiver

```sh
export DIBS_WEBHOOK_SECRET="$(openssl rand -hex 32)"   # also paste this into GitHub
dibs intake serve --listen :8787 --room team --dibs-alias dibs --debounce 10s \
  --ignore-senders dibs-bot
```

| Flag | Default | Meaning |
|---|---|---|
| `--listen` | `:8787` | Address the HTTP server listens on |
| `--room` | `$DIBS_ROOM` | Team room name (case-insensitive, a leading `#` is fine) or room id |
| `--dibs-alias` | `dibs` | Agent the message mentions |
| `--secret` | `$DIBS_WEBHOOK_SECRET` | GitHub webhook secret. **Required** unless you pass `--insecure-no-secret` |
| `--debounce` | `10s` | Batch window. Each new event extends it, up to 3× the window after the first event |
| `--ignore-senders` | `$DIBS_INTAKE_IGNORE_SENDERS` | Comma-separated GitHub logins whose events are dropped (see Security) |
| `--creds` | `~/.dibs/intake.json` | Credentials file from `register` |
| `--log-format` | `text` | `text` or `json` (log/slog) |

Endpoints:

- `POST /webhook` receives GitHub deliveries. It returns `202 queued`, `200 pong` for ping, `200 ignored`, `200 duplicate`,
  `401` for a bad signature, `413` for a body over 1 MB, or `503` while shutting down.
- `GET /healthz` returns `{"status":"ok","pending":N}`.

On SIGINT or SIGTERM the server stops taking new requests and **flushes the pending batch** before it exits.

### Expose it to GitHub

Pick one:

- **smee.io** (fastest for a demo): open https://smee.io/new, then run
  `npx smee-client --url https://smee.io/<channel> --target http://localhost:8787/webhook`.
  Use the smee URL as the GitHub payload URL. Smee forwards the `X-Hub-Signature-256` header unchanged, so signature
  checks still work.
- **cloudflared**: `cloudflared tunnel --url http://localhost:8787`. Use `https://<random>.trycloudflare.com/webhook`
  as the payload URL.

### Configure the GitHub webhook

Go to the repo's **Settings → Webhooks → Add webhook**:

- **Payload URL:** your smee or cloudflared URL (ending in `/webhook` for cloudflared)
- **Content type:** `application/json`
- **Secret:** the same value as `DIBS_WEBHOOK_SECRET`
- **Which events:** *Let me select individual events*, then tick **Issues**, **Pull requests** and
  **Pull request reviews**. Leave everything else unticked. Other events are ignored anyway.

GitHub sends a `ping` straight away. It should show a green check (`200 pong`).

Handled actions:

| Event | Actions |
|---|---|
| `issues` | opened, reopened → `sweep`; labeled, unlabeled, edited, closed → `sync` |
| `pull_request` | opened, reopened, closed (reported as *merged* when merged), synchronize and edited (reported as *updated*) → `sync` |
| `pull_request_review` | submitted (reported as approved / changes requested / reviewed) → `sync` |

### Scheduled messages (weekly report)

`send` posts one message and exits, so you can run it from cron:

```cron
# Mondays 09:00
0 9 * * 1  DIBS_INTAKE_CREDS=$HOME/.dibs/intake.json /usr/local/bin/dibs intake send --room team --text "@dibs report"
```

## Behaviour details

- **Room resolution:** the room name is resolved once with the `list_rooms` operation and then cached. If a send
  returns 404, the cache is cleared, the room is resolved again, and the send is retried once.
- **Retries:** Switch calls are retried with backoff (0.5s, 1s, 2s, 4s) on network errors, 5xx, and 429. Other 4xx
  responses are not retried. The client timeout is 15s per request.
- **Dedupe:** the last 4096 `X-GitHub-Delivery` ids are remembered, so GitHub redeliveries do not post twice. The id
  is only recorded after the signature check passes.
- **Server timeouts:** read-header 5s, read 15s, write 15s, idle 60s. Request bodies are limited to 1 MB.

## Security notes

- **Always set a webhook secret.** Every delivery is checked with HMAC-SHA256 against `X-Hub-Signature-256` using a
  constant-time comparison (`hmac.Equal`). If the signature is missing or wrong, the server returns 401 and queues
  nothing. `serve` refuses to start without a secret unless you pass `--insecure-no-secret`, which is for local
  testing only.
- **Credentials file:** `~/.dibs/intake.json` holds the agent API key. It is written atomically with mode 0600 inside
  a 0700 directory. Do not commit it. On Windows, file modes are not enforced, so keep it in your user profile. If the
  key leaks, re-register with `--overwrite` to rotate it.
- **Secrets never appear in output:** the agent key is only printed masked. Logs never include the key or the
  webhook secret. Prefer environment variables (`SWITCH_REGISTRATION_KEY`, `DIBS_WEBHOOK_SECRET`) over flags, because
  flags end up in shell history and `ps` output.
- **Feedback loops:** if `dibs` labels or comments on issues with its own GitHub account, those changes also trigger
  webhooks. Pass that account in `--ignore-senders`, so intake does not keep asking `@dibs` to `sync` its own changes.
- **Least privilege:** the intake agent only sends messages and lists rooms. It never reads room content, and it
  holds no GitHub token. It only needs to be invited to the team room, not the practice room.
- **Untrusted input:** webhook payloads are only parsed for numbers, actions, and closing keywords. Titles and bodies
  are never copied into the room, so issue text cannot inject instructions into the agents' prompts.
