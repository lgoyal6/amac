# amac

A control plane for the AI coding agents running on your Mac. Every session and
every automation on one page, on your phone, over Tailscale. Written for one
machine, mine, and made to run on yours.

[![ci](https://github.com/lgoyal6/amac/actions/workflows/ci.yml/badge.svg)](https://github.com/lgoyal6/amac/actions/workflows/ci.yml)
[![licence: AGPL-3.0](https://img.shields.io/badge/licence-AGPL--3.0-blue.svg)](LICENSE)
[![go](https://img.shields.io/badge/go-1.26%2B-00ADD8.svg)](go.mod)

```bash
git clone https://github.com/lgoyal6/amac && cd amac
go build -o bin/amac ./cmd/amac

bin/amac hooks -install     # so your agents tell amac what they are doing
bin/amac init               # declare what your automations are meant to deliver
bin/amac daemon             # the board, bound to your tailnet and nothing else
bin/amac url                # the link, token included, to open on your phone
```

Go 1.26+, tmux, and Tailscale for the board. Discord only if you want a phone
notification; everything works without it. Nothing runs in a cloud you pay for.

## What it does that the terminal cannot

- Which session is blocked, and for how long.
- Which automation stopped *delivering*, as opposed to stopped running.
- A queue agents pull from, one claim per task, proved against SIGKILL.
- The uncommitted diff an agent has actually produced, as opposed to what it
  says it produced.
- What all of it costs.

Agents are not scarce any more; attention is. At any moment there are several
Claude Code and Codex sessions running here, and the hard part is no longer
starting them. It is knowing which one is blocked, what it is about to do, what
it is costing, and being able to answer it from wherever I am.

amac is the layer macOS does not have for that. It is called an OS in the sense
ROS is: not a kernel, but the process model, scheduler, permission system and
journal for a class of thing the host does not manage.

**The one design decision: everything is an event.** Every subsystem appends to
one append-only log and subscribes to it; nothing queries another subsystem
directly. The board is a view over the log, automations are subscribers, and
"why did it do that" is answered by replay rather than by guessing.

## The board

One page on the tailnet showing every agent session on this machine, whatever
started it, with the thing you have not answered at the top. It installs to a
phone's home screen and opens as an app.

```
amac daemon      tailnet only, or it does not start
amac url         the link with the token in it
```

- **board** — every session, its live state, and when that state was
  established. `blocked, asked 4h ago` hands the judgement to you; `blocked`
  alone claims something about right now that the log cannot support.
- **wall** — every session's pane at once, for when the question is which of
  twelve is doing something.
- **queue** and **crew** — work agents pull from, and the org (both below).
- **health** — the automation sweep, read out of the log rather than re-derived.
- **spend** — what the agents cost, by project and by model.

A pane is **mirrored, never parsed**. The board can answer a permission prompt
in a session amac does not own, but nothing infers what the prompt says: the
bytes go to your phone, you read the options with your own eyes, and you press
the key you want sent. A mirror cannot be confidently wrong, because it makes no
claim. A row of Allow/Deny buttons over a parsed pane can approve the thing you
rejected.

It also reads the uncommitted diff and browses the session's directory, because
an agent that says it fixed the bug and one that has fixed the bug look
identical in a terminal.

## Automation health

Automations are declared in `~/.amac/health.json` with the cadence they are
expected to *deliver* at. A sweep every 15 minutes records a verdict for each
into the log.

```
amac init                   write a starter roster, then validate it
amac health                 check now and print
amac health -alert -quiet   DM only what changed
amac health -runs           every individual run, not just the newest
```

Two things make this a monitor rather than a log:

**It counts deliveries, not runs.** These pipelines are scheduled several times
over for redundancy, so most runs deliberately do nothing and "the last run was
green" is almost always true and almost never informative. Each probe reads the
artifact the automation commits only once work actually landed.

**It detects silence.** A push-based log records the runs that happened and can
never tell you about the run that didn't. Cadence and grace are declared per
automation and the lateness test is applied centrally, so an automation that
dies quietly still produces a finding.

A probe that fails reports `unknown`, never `ok`. For jobs on a box amac cannot
reach, one line is the whole integration:

```bash
curl -X POST -H "X-Amac-Token: $TOKEN" https://your-mac:7788/api/beat/vps-backup
```

A red line can be handed straight to an agent from the health tab, opened in the
directory the automation actually lives in — which comes from the registry, never
inferred from the name.

## Attention

Every signal that means "this session wants you" lands on `amac attention`,
which decides whether to interrupt, delivers it if so, and records the decision
either way — including the suppressed ones and the reason.

```
amac attention -claude                     from Claude Code's hooks
amac attention -codex '<notify payload>'   from Codex's notify hook
amac attention -bell -session S            from tmux's alert-bell hook
amac hooks [-install]                      report which of those reach amac
```

**Suppress on focus, not on presence.** The predecessor held a notification back
whenever a tmux client was attached or the keyboard had been touched recently.
Both are true nearly always, so every decision was correct and every alert was
silent, for nine days. amac resolves the frontmost terminal tab to a single tmux
session and suppresses only that one. If the answer cannot be determined the
notification is sent: a spurious ping costs a glance, a missed one costs a
blocked agent nobody notices.

`amac hooks` exists because a subsystem that broke and a wire that was never
connected look identical from a phone. It reports, per agent, which signals
actually reach amac.

## A queue, and the org

The org is a chain — planner, executor, verifier — which is right when a task
has stages and wrong when there are simply several unrelated things to do.

```
amac task add <title>            file work
amac task claim [-open]          take the next one, optionally opening a session
amac task done -token N <id>     finish it, if you still hold it

amac crew -plan <task>           print the chain, open nothing
amac crew       <task>           open the first role as a session you can take over
```

A claim is a conditional `UPDATE`, not a read of the log: the log answers "what
happened", the table answers "who holds this right now", and only the second has
to be atomic. Every claim carries a fencing token, so a worker whose lease
lapsed has its result rejected on arrival rather than trusted because it
arrived. Proved by causing it — 120 tasks, 6 workers SIGKILLed holding 5 each,
120 finished exactly once, 0 duplicated.

The crew's handoff is a file, not a pipe, which means you can read the plan on
your phone and decide whether the executor should ever see it.

## amac, to the agents

Everything above points one way: amac watches agents and reports to a human.
`amac mcp` turns it around. An agent in a repo otherwise has no idea whether
another agent is already editing the same tree, or whether the pipeline whose
output it is about to trust delivered this morning.

| tool | when an agent should reach for it |
| --- | --- |
| `working_here` | before editing a tree, so two agents do not produce a diff neither can explain |
| `automation_health` | before trusting a file or feed something else generates |
| `file_task` | for work it found and is not going to do |
| `queue` | before starting something substantial another agent may hold |
| `agent_spend` | when choosing a model for a job that will run many times |
| `report_done` | so a scheduled job it owns is missed when it stops |

Read-mostly on purpose. The two that write are additive. Nothing stops a session
or answers a permission request, because an agent approving another agent's tool
call is the entire reason permission prompts reach a human at all.

## Status

Phase 1, running unattended on this machine. A full session runs end to end
against both agents over [ACP](https://agentclientprotocol.com) — an open,
versioned JSON-RPC protocol with adapters for Claude Code, Codex and Gemini CLI.
Tool calls, permission requests and output arrive as structured data, so
`blocked` is a fact rather than a regex over rendered text.

Verified against Claude Agent v0.66.0 and Codex v1.1.14, both protocol v1, both
driven by the same code.

```
amac setup                     install pinned adapters once
amac run -agent codex 'task'   start a session, send a prompt, answer it
amac probe -all                handshake every agent, record capabilities
amac cost                      what amac's own sessions cost
amac log -n 20                 recent events
```

**Roadmap.** Session babysitting is out of scope; Claude Code Remote Control
ships it. What is left is what Remote Control does not reach.

1. ~~**Daemon**: supervise sessions, API, board on the tailnet~~ — shipped
2. **Gateway and router**: the cascade is in and the harness now measures it
   without showing it the answer key; what is left is the curve against real
   models rather than stubs
3. **Orchestrator**: grade the prompt, convene as many agents as it warrants,
   with a per-task token budget
4. **Sensors**: browser extension and email parsing for an application tracker
5. **Observer**: what I am working on, metadata first, pixels only for
   allowlisted apps
6. **Miner**: patterns in the log become suggested automations

## Does this run on my machine

**macOS** is what it was built for and where all of it works.

**Linux** runs most of it. Declare local jobs with `systemd_unit` rather than
`launchd_marker`; systemd is the better source, because it records when a run
ended and launchd only records how it ended. What Linux loses is one thing, and
it fails safe: focus-based suppression needs the macOS window server, so
elsewhere the notification is simply sent.

**Windows** builds and is not worth running. Use WSL, where it is Linux.

| | macOS | Linux | Windows |
| --- | --- | --- | --- |
| board, queue, MCP, spend | yes | yes | yes |
| tmux sessions, agent hooks | yes | yes | WSL |
| local scheduled jobs | launchd | systemd | no |
| hosted jobs, heartbeats | yes | yes | yes |
| suppress on focus | yes | always notifies | always notifies |
| keychain credentials | yes | env vars | env vars |

## More

- [How it works, and why](docs/design.md) — the long version: what each
  subsystem is guarding against, and the bug that put it there
- [Contributing](CONTRIBUTING.md)

## Licence

[AGPL-3.0](LICENSE). Use it, change it, run it. If you distribute it **or run a modified
version as a network service**, that version has to be open source too, and the
licence carries an express patent grant from every contributor. If you want it
under other terms, ask.
