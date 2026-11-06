# How Dibs uses Switch

> **Delete Switch and Dibs has no room to enforce a rule in, no agent identities to hold to it, and no tamper-proof place to keep the evidence.**

Dibs is a set of Switch rooms plus a small rules engine. The engine (the `dibs` binary) decides. Switch is where the people, the agents and the rules meet. This page lists each Switch primitive Dibs depends on, what it does in Dibs, and what happens if you take it away.

## Summary

| # | Switch primitive | What Dibs does with it | What breaks without it |
|---|---|---|---|
| 1 | Rooms on Discord (bridge) | `#payments-team` and `#asha-practice` live in the Discord server the team already uses | People and agents have no shared space; you'd build a bot per chat app |
| 2 | Room instructions | The Dibs Protocol is the instructions of both rooms, so every agent gets it on join | Each vendor tool needs its own config, and they drift |
| 3 | Group + room links | Group `dibs-payments`; link team → practice with label `practice` | Agents can't tell the private room belongs to the team |
| 4 | Room documents, author-locked | `skill-map-asha` and `dibs-ledger`, writable only by `dibs` | The skill record is editable by anyone, so it proves nothing |
| 5 | Addressing rules | Agents answer people in their rooms; the senior is not in the practice room | Anyone anywhere could drive the agents |
| 6 | Native approval prompts | `dibs approve` asks **Allow once / Decline**; the owner (the senior) answers | You'd write a custom approval system and its audit trail |
| 7 | Templates (v1) | `switch/dibs.template.yaml` creates 3 agents, 2 rooms, the group, the link, instructions, docs and kickoffs | Setup takes an hour by hand and nobody can reproduce it |
| 8 | Multiple vendors | `dibs` and `builder` on Claude Code, `reviewer` on Codex, in the same rooms | No way to show "one rule, many agents" |
| 9 | Agent protocol | `dibs-intake`, a non-LLM program, joins as a member and relays GitHub issues | A separate webhook bot per chat app |
| 10 | Agent ops | `post_message`, `read_context`, `create_room_document`, `update_room_document` | The agent can't publish the record or read the room it coaches in |

---

## 1. Rooms on Discord, through the Switch bridge

Switch rooms are bridged to a channel on a messaging app. Dibs uses two:

- **`#payments-team`** (public channel). Ravi (senior), Asha (junior), and the three agents. Triage cards, approvals, builder PRs, teaching moments and reports land here.
- **`#asha-practice`** (private channel). Asha and the three agents. This is where Asha asks for hints.

Nobody installs a new app. The senior approves from the same Discord they already have open, and the junior asks questions where they already chat.

Setup detail that matters: each person links their Discord account to their Switch user. An unlinked Discord account matches nobody, so the agents can't tell who is talking. Dibs relies on the sender identity to check that only a senior approves.

**Without it:** you need a bot per chat platform, your own user mapping, and your own message history.

## 2. Room instructions

Every Switch room has instructions that each agent reads when it joins. Dibs puts the **Dibs Protocol v1** there, word for word, in both rooms. The key rules:

1. A ticket labelled `dibs:reserved:<person>` belongs to that person. No agent writes, pastes, commits or pushes code that solves it, and no agent opens a PR that closes it.
2. Before any help on a reserved ticket, run `dibs hint request N --level L --agent <you>`. Give help only at the level it grants.
3. After writing the hint, run `dibs hint give N --level L --agent <you> --text-file F`. The binary checks the text and only then returns the message to post.
4. When the person describes something they tried, run `dibs attempt N --by <person> --note "..."` first.
5. Never shame. No "simply", "just", "obviously", "easy".
6. Release is not failure.
7. Practice-room messages are never quoted in the team room. Summaries only.

The point: **the rule is attached to the room, not to an agent.** Claude Code and Codex read the same text from the same place. Add a third vendor tomorrow and it gets the same rule on join, with no per-tool config.

**Without it:** you'd maintain a `CLAUDE.md` for one agent, an `AGENTS.md` for the other, and hope they match. There is no single source of truth, and no guarantee a new agent ever sees the rule.

## 3. Group and room links

The template creates a group `dibs-payments` and a link:

```yaml
links:
  - { from: "{team}-team", to: "{junior_handle}-practice", label: practice }
```

The link tells `dibs` that `asha-practice` is Asha's practice room for this team. When a reserved ticket merges, `dibs` posts the evidence in both rooms and pushes the skill map document to both. When Asha's ticket is released, `dibs` tells them in the practice room and tells the team in the team room.

**Without it:** the private room is just another channel. Agents would need a hardcoded mapping, and a second junior would mean editing it by hand.

## 4. Room documents with enforced authorship

Switch room documents have an author, and **only the agent that created a document can change it**. Dibs uses three:

| Document | Room(s) | Author | Content |
|---|---|---|---|
| `dibs-protocol` | team | template | The protocol, for reference |
| `skill-map-asha` | team + practice | `dibs` | Each skill, its level (Unseen → Fluent), and the PR that moved it |
| `dibs-ledger` | team | `dibs` | Open reservations and the last 20 events |

After any state change, `dibs mirror` prints the markdown. The `dibs` agent pushes it with `create_room_document` the first time and `update_room_document` after.

Asha can read their skill map but can't edit it. Neither can `builder` or `reviewer`. The senior can't quietly edit it through another agent either. That makes the document evidence: every level change traces to a merged PR, a hint depth and a review count in the ledger.

**Without it:** the skill record is a wiki page anyone can change, and a growth record anyone can edit is worth nothing.

## 5. Addressing rules

Each agent is set to `addressing: anyone`, which in Switch means anyone **in the rooms that agent belongs to**. The senior is not added to the practice room. So:

- Asha can address `@builder` and `@reviewer` in their practice room and get hints.
- People outside Dibs' rooms can't drive these agents.
- Senior-only actions (`approve`, `decline`, `unlock`, `budget`) are checked again by the binary against `.dibs/team.yaml`. If Asha types `@dibs ok 3`, the binary refuses with exit code `2` and the message *"only a senior can do that; asha is a junior"*

The practice room is the junior's own space. The senior stays out of it by default and sees only summaries in the team room, so the junior can ask anything without an audience.

**Without it:** any member of the workspace could tell `builder` to paste the solution.

## 6. Native approval prompts

The `dibs` agent's Claude Code settings mark one command as needing permission:

```json
{ "permissions": {
    "allow": ["Bash(dibs:*)", "Bash(gh:*)", "Bash(git:*)"],
    "ask":   ["Bash(dibs approve:*)"] } }
```

When `dibs` runs `dibs approve 3 --by ravi`, Switch shows an **Allow once / Decline** prompt in the team room. The agent's owner answers it. Ravi deployed the template, so they own the agent and are the one who answers. Switch records who answered.

That gives Dibs a real human gate on the one decision that changes someone's week: holding a ticket back from the agents for a junior. One tap.

Fallback: if the prompt does not show up in a given bridge setup, Ravi types `@dibs ok 3` and the binary checks the sender against the seniors in `team.yaml`. Same rule, slower path.

**Without it:** you'd build approvals, identity checks and an audit trail yourself.

## 7. Templates (version 1)

`switch/dibs.template.yaml` is a version 1 template, so it runs on any Switch server, including a local Console-managed Docker server. One import creates:

- 3 agents: `{team}-dibs`, `{team}-builder` (Claude Code), `{team}-reviewer` (Codex), each with instructions, a working directory, the repo and GitHub access
- 2 rooms: `{team}-team` (public) and `{junior_handle}-practice` (private), both with the Dibs Protocol as instructions and aliases `dibs`, `builder`, `reviewer`
- the group `dibs-{team}` and the `practice` link
- the `dibs-protocol` document and a `socratic-reviewer` role
- kickoff messages: `dibs` introduces itself and runs the first sweep

The form asks for five things: team name, the junior's chat user, the junior's handle, the repo, and the bridge. `dibs template --check` validates the YAML before you import it.

**Without it:** an hour of clicking per team, and no way for a judge or another team to reproduce the setup.

## 8. Multiple vendors in one room

`builder` runs on Claude Code. `reviewer` runs on Codex. They sit in the same practice room and both get the same rule from the room. In the demo, Asha asks each for the code to ticket #3. Both refuse, both call `dibs hint request`, and both give a hint at the level the ledger granted.

This is the main reason Dibs is a Switch project. A rule that one vendor's agent follows is a prompt. A rule that agents from two vendors follow, because the room says so, is a team policy.

**Without it:** single-vendor tools can't show "one rule, many agents", and a team that uses both tools has two policies.

## 9. Agent protocol: `dibs-intake`

`dibs-intake` is not an LLM. It is the same `dibs` binary running as a custom Switch agent:

```bash
dibs intake register   # registers with the Gateway (POST /agents) using a registration key, saves its id
dibs intake serve      # receives GitHub issue webhooks
dibs intake send "..." # posts a message into the team room (POST /agents/{id}/message)
```

When a new issue opens, it posts *"@dibs new issues: #3 #6"* in the team room, which wakes `dibs` to triage. It joins as a first-class room member, with its own identity, next to the LLM agents.

**Without it:** a webhook bot per chat platform, or a person typing `@dibs sweep`.

## 10. Agent ops

| Op | Used by | For |
|---|---|---|
| `post_message` | all agents | Posting the `messages` array that each `dibs` command returns, verbatim |
| `read_context` | `dibs`, coaches | Reading recent room history before answering, so hints build on what Asha already tried |
| `create_room_document` | `dibs` | First write of `skill-map-asha` and `dibs-ledger` |
| `update_room_document` | `dibs` | Every later write, after `dibs mirror` |

The `dibs` binary never talks to Switch directly. It returns text; the agent delivers it through these ops. That keeps all Switch access under the agent's identity, which is what makes the document authorship lock meaningful.

---

## What Switch does not do for Dibs (and what does)

| Need | Who handles it |
|---|---|
| Deciding which ticket is a good rep | `dibs` binary (scoring, unit-tested) |
| Deciding how deep a hint may go | `dibs` binary (hint gate) |
| Checking a hint's text before it is posted | `dibs` binary (hint text checker) |
| Stopping someone else from closing a reserved ticket | `dibs guard`, a required GitHub check |
| Scheduling the weekly report | A cron job or a Discord scheduled message that posts `@dibs report` |

Switch provides the room, the identities, the rule's delivery, the approvals and the tamper-evident record. The binary and GitHub provide the hard decisions and the hard stop. Each layer covers what the other can't.
