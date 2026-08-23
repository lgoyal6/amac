# amac

A control plane for the AI agents running on my Mac.

Agents are not scarce any more; attention is. At any moment there are several
Claude Code and Codex sessions running here, and the hard part is no longer
starting them. It is knowing which one is blocked, what it is about to do,
what it is costing, and being able to answer it from wherever I am.

amac is the layer macOS does not have for that. It is called an OS in the
sense ROS is: not a kernel, but the process model, scheduler, permission
system and journal for a class of thing the host does not manage.

| OS concept | amac |
| --- | --- |
| processes | agent sessions |
| scheduler | orchestrator with per-task model budgets |
| syscalls | actuators (Notion, tmux, Discord) |
| capabilities | per-app and per-verb allowlists |
| journal | the event log |
| drivers | one ACP adapter per agent, one gateway per model |

## The one design decision

Everything is an event.

Every subsystem appends to one log and subscribes to it. Nothing queries
another subsystem directly. The dashboard is a view over the log, automations
are subscribers, the workflow miner is a query, and "why did it do that" is
answered by replay rather than by guessing.

That constraint is what makes the rest tractable, and it is the piece that
cannot be retrofitted later.

## Why ACP, and not screen scraping

The predecessor to this (`~/agentmon`) read agent state with `tmux
capture-pane` and regexes over the rendered terminal. It worked, and it taught
me exactly why it cannot be made reliable: permission mode becomes unreadable
the moment a dialog covers the status footer, a pane that merely *discusses* a
prompt trips the same detector as a pane that *has* one, and two separate
detectors were needed because neither was trustworthy alone. All of that is
the cost of parsing a rendering instead of reading a state.

[ACP](https://agentclientprotocol.com) is an open, versioned, JSON-RPC
protocol for exactly this, with adapters for Claude Code, Codex and Gemini
CLI. Tool calls, permission requests and output arrive as structured data.
Being vendor-neutral is not a bonus here, it is the requirement: I use Codex
today and want open models on cheap work tomorrow.

## Status

Phase 1. A full session runs end to end against both agents:

- **ACP client**: bidirectional JSON-RPC over stdio. Concurrent request
  correlation, agent-initiated requests answered on their own goroutines,
  bounded notification delivery, dead agents surface as errors rather than
  hangs.
- **Supervisor**: owns sessions, tracks state from protocol messages, answers
  `fs/read_text_file`, `fs/write_text_file` and `session/request_permission`,
  streams every `session/update` into the log.
- **Event log**: SQLite in WAL mode, append-only, total order by sequence,
  explicit fsync policy, live subscribe plus replay-from-sequence.
- **Adapter registry**: one table, the only vendor coupling in the codebase.

```
amac setup                     install pinned adapters once
amac run -agent codex 'task'   start a session, send a prompt, answer it
amac probe -all                handshake every agent, record capabilities
amac log -n 20                 recent events
```

Verified 2026-08-08 against Claude Agent v0.66.0 and Codex v1.1.14, both
protocol v1, both driven by the same code: session created, prompt sent, tool
calls streamed, permission request raised and answered, files written, turn
ended clean.

### What this bought over the old approach

`blocked` is now a fact. It is set when the agent sends
`session/request_permission`, carries the tool title and the exact options
offered, and is cleared by replying on a channel. The predecessor guessed at
it with a regex over rendered text and needed two competing detectors because
neither was trustworthy.

Cost tracking also comes free: `usage_update` notifications carry token counts
and a `cost` object straight from the protocol, so `amac cost` is a query over
data already on disk rather than a subsystem.

```
SESSION          AGENT    WHEN         COST       CTX  TURNS  ASKS
claude-3c1297    claude   Aug08     $0.1338        3%      1     1
codex-1a4f       codex    Aug08         n/a        7%      1     0

total $0.1338 across 1 priced session(s)
1 session(s) reported no cost (agent does not expose it); total is a lower bound
```

Codex reports tokens but not money, so `Cost` is a `*float64` and an unpriced
session prints `n/a`. Coercing it to `$0.00` would produce a report that
silently understates spend, which is the one thing a cost report must never
do.

## Automation health

The first subsystem that runs unattended. Five automations are declared with
the cadence they are expected to *deliver* at, and a launchd sweep every 15
minutes records a verdict for each into the log.

```
amac health                 check now and print
amac health -alert -quiet   DM only what changed (launchd, every 15m)
amac health -digest         DM the whole roster (launchd, 10:00 daily)
```

Two things make this a monitor rather than a log.

**It counts deliveries, not runs.** Every one of these pipelines is scheduled
several times over for redundancy, so most runs deliberately do nothing:
morning-brief fires four crons and the first success claims the day, hacklist
fires four and a gate lets one through. "The last run was green" is therefore
almost always true and almost never informative. Each probe reads the artifact
the automation commits only once work landed: `briefs/.delivery.json`, written
after Discord confirms the send; `data/history/sweep-<ISO>.json`, written by a
real sweep. The two local launchd jobs are read the same way, from the
completion marker they append to their log rather than from the file's mtime,
because a job that dies halfway still writes to its log.

**It detects silence.** A push-based log records the runs that happened and can
never tell you about the run that didn't. Cadence and grace are declared per
automation and the lateness test is applied centrally, so an automation that
dies quietly still produces a finding, and no probe has to remember to check.

A probe that fails reports `unknown`, never `ok`. A monitor that says green
when it only proved the web server answers is worse than no monitor, because it
converts an unknown into a false assurance.

## Roadmap

Session babysitting is **out of scope**. Claude Code Remote Control now pushes
to the phone when a task finishes or needs a decision, takes the approval from
there, and suppresses on terminal *focus* rather than mere presence, which is
the bug that made the tmux predecessor useless in practice. Rebuilding that
would be rebuilding a feature the vendor ships. What is left is what Remote
Control does not reach: Codex and Gemini sessions, automation health, and cost
across all of them.

1. **Daemon**: supervise sessions, WebSocket API, widget dashboard on the
   tailnet
2. **Gateway and router**: LiteLLM in front of Anthropic and open models. A
   cascade, not a predictor: strong model by default, route down only on
   high-confidence-easy plus mechanical verification, escalate on doubt. The
   evaluation harness lands before the router, because the measured
   cost/quality curve is the only claim worth making
3. **Orchestrator**: grade the prompt, convene as many specialised agents as
   it actually warrants, with a per-task token budget
4. **Sensors**: browser extension plus email parsing to keep an application
   tracker current without being told
5. **Observer**: what I am working on, metadata first, pixels only for
   allowlisted apps, default deny
6. **Miner**: patterns in the log become suggested automations

## Design notes

**Durability is a choice, not a default, and the cost is measured.** `Full`
fsyncs every commit, so an acknowledged event has reached the disk. `Relaxed`
lets the OS schedule it. On this machine (M-series, APFS):

| policy | ns/op | appends/sec |
| --- | --- | --- |
| Full | 71,056 | ~14,100 |
| Relaxed | 54,168 | ~18,500 |

31% for the guarantee, against a workload that peaks in the low hundreds of
events per second. Full is the default and it is not close.

`TestCrashDurability` proves the guarantee rather than asserting it: it forks a
child that appends and reports each acknowledged sequence, SIGKILLs it
mid-write, reopens the database, and checks that every acknowledged event is
present, that no record is torn, that no payload is corrupt, and that the
sequence continues forward rather than being reused.

**A slow subscriber gets dropped, never blocks the writer.** The log is the
durable record; a live subscription is a convenience. Losing the second must
never risk the first, and a dropped subscriber recovers by replaying from its
last sequence number.

**Scanner buffers are sized for real payloads.** `bufio.Scanner` defaults to
64KB, and a single tool result carrying a file blows straight past it. The
failure mode is indistinguishable from an agent going quiet, which is the
worst kind of bug to debug, so the limit is explicit and overflow is a hard
error.

**Capabilities stay as raw JSON.** Adapters disagree about what they
advertise and gain new capabilities between releases. Decoding into a fixed
struct would turn a new capability into a broken handshake.
