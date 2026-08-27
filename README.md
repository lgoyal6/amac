# amac

A control plane for the AI coding agents running on your Mac. Every session and
every automation on one page, on your phone, over Tailscale. Written for one
machine, mine, and made to run on yours.

[![ci](https://github.com/lgoyal6/amac/actions/workflows/ci.yml/badge.svg)](https://github.com/lgoyal6/amac/actions/workflows/ci.yml)

```bash
git clone https://github.com/lgoyal6/amac && cd amac
go build -o bin/amac ./cmd/amac

bin/amac hooks -install     # so your agents tell amac what they are doing
bin/amac init               # declare what your automations are meant to deliver
bin/amac daemon             # the board, bound to your tailnet and nothing else
bin/amac url                # the link, token included, to open on your phone
```

AGPL-3.0. Use it, change it, run it. If you distribute it **or run a modified
version as a network service**, that version has to be open source too, and the
licence carries an express patent grant from every contributor. If you want it
under other terms, ask.

macOS, Go 1.26+, tmux. Tailscale for the board; Discord only if you want a phone
notification, and everything works without it. Nothing runs in a cloud you pay
for.

**What it does that the terminal cannot.** Which session is blocked and for how
long. Which automation stopped delivering, as opposed to stopped running. A
queue agents pull from, one claim per task, proved against SIGKILL. The
uncommitted diff an agent has actually produced, as opposed to what it says.
What all of it costs.

---

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
amac init                      write and validate the health roster
amac hooks -install            wire the agents' own signals into amac
amac daemon                    the board, on the tailnet
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

The first subsystem that runs unattended. Automations are declared in
`~/.amac/health.json` with the cadence they are expected to *deliver* at, and a
launchd sweep every 15 minutes records a verdict for each into the log. Ten are
declared on this machine.

```
amac init                   write a starter roster, then validate it
```

The roster used to be ten Go constants: my repos, my launchd labels, my log
paths, compiled in. That was the single reason nobody else could run the part of
amac that watches automations, which is most of what it does when nobody is
looking at it. There is deliberately no built-in fallback: a default list of
someone else's automations would probe paths that do not exist and report a
healthy machine as broken, and an empty fallback would sweep nothing while
reporting success. A missing roster is an error naming the file and the command
that writes one.

Seven probe shapes cover the ten, and the split is honest rather than tidy. Four
are a launchd job with a completion marker in a log, and they shared an
implementation before the roster made that visible. What stayed separate is how
a given pipeline reports itself: a marker carrying a date and no time,
timestamps encoded in filenames, an n8n API. Those take their repo or workflow
as a parameter, because flattening them into one shape would describe the world
less accurately than naming three does.

A bad roster reports every problem it has at once rather than the first, and one
bad entry fails the whole load. An automation silently dropped is an automation
nobody is watching.

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

A probe that fails reports `unknown`, never `ok`.

### Preventions, and services

Three of the nine are not deliveries in the sense above, and each needed the
probe to answer a different question.

**A prevention delivers the absence of a problem.** `tmux-idle-reaper` kills
detached agent sessions idle beyond eight hours, and its normal run correctly
does nothing, so its log was silent whether it was working or had stopped. A
prevention that has quietly stopped preventing is invisible by construction:
the symptom is sessions accumulating over days, not an error anywhere. It now
writes a completion marker on every run with the count on it, so "nothing
needed reaping" and "the reaper is gone" are finally different facts.

Declaring it caught a real fault within the hour, which is the argument for
declaring things. Its log lived under `~/Desktop`, which is TCC-protected, so a
launchd agent writing there fails with `operation not permitted`. The kill
worked and the record of it did not, and while the script only logged on a kill
there was nothing to notice: the reaper could have ended sessions and left no
trace at all. It logs to `~/Library/Logs` now, where the other local jobs
already write successfully.

The count is why the reaper is the one job whose *runs* are filtered rather than
reported. Forty-eight a day saying nothing happened is how a channel gets muted,
and this system already paid for that lesson. A run that actually killed a
session is a session ended on this machine without anyone asking, so that one
gets a line.

**Detection and deletion want different clocks.** Swap and disk were computed
only by the cache job, which ran weekly. Swap crossed 90% here on a Tuesday and
the next thing that would have looked was Sunday's run. So the reading moved
onto the reaper's thirty-minute tick, which already existed and already walked
the machine, and the cache job moved to daily. Neither job decides anything: the
reaper records the numbers in its marker and `machine-pressure` decides whether
they are worth waking someone for, which keeps alerting in one place.

It is a separate line from the reaper rather than a note on it, because "the
reaper script is healthy" and "the machine is drowning" have different fixes and
a single red line covering both sends you to read a shell script when what is
needed is closing something. For the same reason the report names which limit
was crossed:

```
machine-pressure  failing  swap 92% and disk 91%, read 53s ago
                  · swap is memory, not disk: clearing caches will not move it,
                    closing sessions will
```

That note exists because the obvious response to pressure is to run the cache
job, and for swap that response does nothing at all. Nothing the sweep deletes
is in memory.

**A banner is not a delivery, and this applied to more than it looked like.**
These logs carry two markers, `local passes starting` and `local passes done`,
and reading the newest one conflated them. The rule went in for `disk-sweep` and
then `hacklist-local-passes` was caught by it live: the fix for its crash landed,
the 20:30 run started, and the probe reported "last completed 2m ago" about a job
that had started 2m ago and was still running. The same reading after a crash
reports the start of the run that died as a delivery.

So every local probe now separates them. The last completion stays the delivery,
which keeps the lateness test measuring deliveries rather than attempts, and a
start with nothing after it is read against launchd: still executing is
`running since`, no longer executing is a death mid-run, which is invisible to
anything that reads only the newest line.

Declaring both jobs in one registry is what made their overlap visible.
`disk-sweep` also killed idle sessions, on a weekly tick and a seven-day
threshold, and the reaper kills anything detached and idle past eight hours. So
the only sessions the weekly rule could still reach were the ones the reaper had
spared on purpose for being active: it could not do its stated job and could
kill a working agent. Worse, it measured `session_created` while its own header
said idle, and age is not idleness for the one case that matters, a session
started nine days ago and working right now. Sessions now have one owner. The
weekly pass measures idleness, and only when run by hand.

**A service is either up or it is not.** `amac-daemon` serves the board, and
"when did it last deliver" is the wrong question about it, so its probe checks
liveness directly and leaves `Last` zero, which suppresses the cadence test
rather than inventing a delivery to satisfy it. That is the stronger check: a
silent job is only detectable after cadence plus grace, a dead service on the
next sweep. It separates the three failures because they are fixed differently,
one of which is not a failure of the daemon at all:

```
amac-daemon  down  not serving: Tailscale is not up, and the daemon binds
                   the tailnet or nothing
```

`devspend` is the weakest probe here and labels itself so. The script writes no
completion marker, so the only evidence is launchd's exit status and the log's
mtime, which cannot see a job that died after its first line of output. Watched
anyway, because an unwatched automation is worse than a weakly watched one, and
the report never claims more than it proved.

### Every run, not just the newest

The sweep above asks "is this delivering?", which is a question about the
newest state. That is right for waking someone up and wrong for noticing a
failure that was recovered from: job-discovery crashed three times in twenty
hours while the sweep reported it green throughout, because a success followed
each crash before anyone looked.

`amac health -runs` reports each individual run exactly once, whatever happened
after it. Telling a real skip from a failure needs a different signal per
automation, because none of them expose it the same way:

| automation | how a skip is identified |
| --- | --- |
| morning-brief | GitHub reports every step `success` either way, because the skip happens *inside* the steps. So the test is whether a delivery commit landed inside the run's own window. |
| hacklist-sf | the gate skips a whole job, which GitHub does report: `job=discover conclusion=skipped`. |
| job-discovery | a run with nothing to send still succeeds. Its pipeline report carries `shouldSend` and the counts behind it, which also makes the better message: "0 accepted of 61,268 scanned". |
| launchd jobs | each completion marker is one run; brew prints its own failure tally. |

Failures are sent on their own so they are never a line in a list you skim.
Everything else arrives as one batch per sweep, because one message per run is
about twenty-two pings a day, and a channel that pings twenty-two times a day
gets muted. That is not a matter of taste: a muted channel is exactly how
nothing reached the phone between Aug 13 and Aug 22.

The first sweep records every run the APIs still remember, dozens of them, and
sends none of it. Announcing history he has already lived through would bury
the one thing this exists to surface. A monitor that says green
when it only proved the web server answers is worse than no monitor, because it
converts an unknown into a false assurance.

## Attention

Every signal that means "this session wants you" lands on `amac attention`,
which decides whether to interrupt, delivers it if so, and records the decision
either way.

```
amac attention -claude                     from Claude Code's hooks, payload on stdin
amac attention -codex '<notify payload>'   from Codex's notify hook
amac attention -bell -session S            from tmux's alert-bell hook
amac hooks [-install]                      report which of those actually reach amac
```

**Claude was out of scope, and that was wrong for two weeks.** The reasoning
was sound in the abstract: Remote Control covers Claude Code, and rebuilding a
feature the vendor ships is waste. What it missed is that Claude Code's hooks
on this machine were still pointing at the predecessor, whose suppression rule
was the one documented below as having produced nine days of silence. Codex
came through, Claude did not, and nothing anywhere said so. The log settles it:
94 attention events on record and every single one of them Codex.

Two failures look identical from a phone, and this was the second kind: not a
subsystem that broke, a wire that was never connected. So the fix is not only
the `-claude` path but `amac hooks`, which reports per agent which signals
actually reach amac and which do not. A control plane that cannot say whether
its own inputs are connected will lie by omission, and it will do it quietly.

```
claude
  ok  Notification       blocked: waiting on you, and says what for
  ok  Stop               idle: turn finished, carries what it said
  ok  PostToolUse        working: clears blocked once you approve
codex
  ok  notify             idle: turn finished, carries what it said
  ok  alert-bell         blocked: the only signal Codex has for it
```

**Claude's hooks are the shape Codex's should have been.** Everything below
about bells and four-second races is a workaround for a signal that does not
say what it means. `Notification` fires exactly when Claude is waiting on a
human *and says what it is waiting for*; `Stop` fires when a turn ends. Each
means one thing, so neither is coalesced and no bell is involved. What Claude
does not hand over is the assistant's last message, so it is read out of the
transcript the hook names: a file the agent wrote, not a rendering of a
terminal, which is the same class of source as the ACP wire.

**Most hooks are worth showing and not worth interrupting anyone over.** State
and delivery are decided separately: `UserPromptSubmit`, `PostToolUse`,
`SessionStart` and `SessionEnd` update the board and ring nothing. That split
is what finally gives the board a live state for sessions amac did not start,
and `PostToolUse` in particular is the only thing that clears `blocked` before
the turn ends. Without it a session reads as waiting on you for the whole run
after you have already approved the tool, which is exactly the confidently
wrong state this system exists to stop producing. Since it fires on every tool
call, an unchanged state writes nothing: a heartbeat in the log is a log nobody
can read.

agentmon's hooks were left in place rather than replaced. They feed tooling
still in daily use, their own presence check suppresses their push nearly
always, and breaking a working tool to install a new one is not an upgrade.

**Two signals, because Codex only offers one and it is the less useful one.**
The `notify` hook fires solely on `agent-turn-complete`, so a request for
approval, the case where the session is actually blocked, never reaches a
program that way. It does ring the terminal bell, and tmux can turn a bell into
a hook. That is the only structured route to "this session is waiting on you"
that does not involve scraping the pane, which is what ACP was adopted to stop
doing.

A finished turn produces both signals within the same second. The bell handler
therefore waits four seconds before deciding, which lets the notify hook, the
one that knows what the agent actually said, land first and win. An approval
request produces only the bell, so after four seconds nothing has superseded it
and it goes out.

**Suppress on focus, not on presence.** The predecessor held a notification
back whenever a tmux client was attached to the session or the keyboard had
been touched in the last two minutes. Both are true nearly always: a dozen
clients are attached right now and he is at the machine all day. So every
decision was correct and every alert was silent, for nine days. This resolves
the frontmost Terminal tab to its tty, maps that to a tmux client, and
suppresses only that one session. Everything else gets through. If the answer
cannot be determined the notification is sent, because a spurious ping costs a
glance and a missed one costs a blocked agent nobody notices.

Every decision is recorded, including the suppressed ones and the reason. A
notification that was correctly withheld and one that silently failed look
identical from outside, and telling them apart is precisely what was impossible
before.

## Crew: the org, as sessions you can take over

`amac do` runs the roles headless. `amac crew` runs the same org as real tmux
sessions you can attach to, argue with, and carry on from inside.

```
amac crew -plan  <task>    print the chain, open nothing
amac crew        <task>    open the first role
amac crew -next  <task>    open the next role whose input is ready
```

```
team  (graded by deepseek-v3)
handoff  ~/.amac/runs/add-a-json-flag-to-amac-health

  1. planner   claude  ready     am-add-a-json-flag-to-amac-health-planner
  2. executor  claude  waiting   am-add-a-json-flag-to-amac-health-executor
  3. verifier  codex   waiting   am-add-a-json-flag-to-amac-health-verifier
  4. reviewer  codex   waiting   am-add-a-json-flag-to-amac-health-reviewer
```

**The handoff is a file, not a pipe.** Headless, a role's output is read off the
ACP wire and passed to the next in memory. Once a human has the keyboard that
is gone, and the only way to recover it would be to read the rendered pane,
which is the practice ACP was adopted to end. So each role writes to
`~/.amac/runs/<slug>/<role>.md` and the next is told to read it. The chain
advances only as far as the artifacts exist, which is why roles show `waiting`
rather than opening and burning context on a file that is not there yet.

Making the handoff an artifact buys something the in-memory version could not:
the plan can be edited before the executor ever sees it, and a run reads back
afterwards without the sessions still being alive.

Sessions are named `am-<slug>-<role>`, following the convention the rest of the
machine already uses, so they appear in existing tooling and not only here.

## The board

One page on the tailnet showing every agent session on this machine, whatever
started it, with the thing you have not answered at the top.

```
amac daemon                    tailnet only, or it does not start
                               launchd: com.amac.daemon, restarts itself
```

`amac url` prints the link with the token in it, because assembling it by hand
meant knowing the port, finding the tailnet address and catting the token, and
the device that needs the link is the one that cannot do any of those.

A device that opens the board without a token gets a page that says so. It used
to get working chrome, an empty board and a connection pill reading
`reconnecting`, which points at the network rather than at the actual cause. An
empty board and an unauthorised one looked identical, which is this system's
recurring bug wearing a different hat.

It installs to a home screen: a manifest, an icon and `display: standalone`, so
it opens as an app rather than a tab. Those three files are the only things
served without a token, because iOS fetches them while adding to the home screen
from a context that has none of the page's storage, and neither carries any data
about this machine.

Discord is a good delivery channel and a bad control surface. It can tell you a
session is blocked; it cannot show you what the session is asking, and it
cannot answer for you. The board is the other half.

**Three tabs, because there were three things worth walking to the machine
for.** *board* is every ACP and tmux session with its live state; *crew* runs
the org (below); *health* is the automation sweep the Discord digest already
sends, read straight out of the log rather than re-derived, because two
implementations of "is this delivering" that can disagree is worse than one.

**A pane is mirrored, never parsed.** The board can answer a permission prompt
in a session amac does not own, which sounds like exactly the screen-scraping
this codebase refuses to do. The rule is about inference, not pixels. Nothing
reads the pane: the bytes are forwarded to a phone, a human reads the options
with their own eyes as they would three seconds after `tmux attach`, and presses
the key they want sent. A mirror cannot be confidently wrong, because it makes
no claim.

The alternative on offer was a row of Allow/Deny buttons over a parsed pane.
That is the predecessor's bug in a nicer coat, and its failure mode is worse
than the predecessor's: a wrong guess about *state* shows a wrong badge, while a
wrong guess about *which option is which* approves something you rejected.

**A suppressed duplicate is not evidence.** A finished Codex turn fires both
signals inside the same second: the notify hook, which knows what the agent
said, and the terminal bell, which knows only that something happened. amac
recognises the bell as a duplicate and withholds it, and that was working
exactly as designed. The board then read the newest attention event regardless,
so the withheld bell overwrote the turn-complete it duplicated and the card read
`blocked`. Not for a moment: until the next turn, which for a session waiting on
a human is indefinitely.

Five of seven cards were claiming to want something. Two actually did. The rule
now is that a signal held back *because it duplicates another* is skipped and
the one it duplicated is used; every other suppression is left alone, because
"you are looking at it" means the session genuinely did ask and amac merely
declined to interrupt.

**Reading the work, not just the talk.** The pane shows what an agent is saying.
An agent that says it fixed the bug and one that has fixed the bug look
identical in a terminal, so the board also reads the uncommitted diff and
browses the session's directory. Read-only, confined to that directory, with
both sides of the path symlink-resolved before they are compared: a prefix check
on the unresolved string passes for a link inside the root pointing anywhere on
the disk, and this is reachable from a phone.

**State is dated, because state is evidence.** It still comes only from hooks, so
a session whose agent has no hooks wired reports `unknown`, and that remains the
correct answer rather than a gap. The subtler case is Codex, which has no signal
for "the human answered": a session it reported blocked stays blocked on the
board until its next turn ends, which can be hours. So every card carries when
its state was established. `blocked, asked 4h ago` hands that judgement to the
person reading it. `blocked` alone makes a claim about right now that nothing in
the log supports.

**Every keystroke is an actuation.** Typing into a pane from a phone is
indistinguishable at the terminal from having typed it there, so the log is the
only place that can say which one happened. Named keys are an allowlist and
everything else is sent literally, because `tmux send-keys` reads "Enter" in a
sentence as a keypress: asking an agent about a keybinding would otherwise press
it.

**Forcing a locale on tmux, which is not the kind of bug you expect.** The board
went live on the tailnet and showed nothing, with seventeen sessions running.
tmux sanitises control characters out of its own `-F` output when the locale is
not UTF-8, so the tab separator comes back as an underscore:

```
am-amac_1787595768_1787596187_1
```

Every line then has one field instead of four, every session is dropped, and the
caller is handed an empty list and no error. An interactive shell sets `LANG`, so
it never happens while you are testing; launchd gives an agent none, which is
exactly where the daemon runs. The locale is forced at the point tmux is read
rather than in the plist, because it is a property of reading tmux and not of
how amac happens to be started.

The second half of that bug was `List` swallowing every error into an empty
result. An empty board and an unreadable tmux look identical on screen, so the
failure is now returned and recorded.

**The bind is fail-closed and now tells you why.** The daemon starts agents,
approves their tool calls and types into panes, so it binds the tailnet or it
does not start. `tailscale ip` reports this node's address even while the client
is stopped, because the control plane assigned it rather than this machine, so
the address is now checked against the local interfaces before it is trusted.
Without that the daemon passed its own safety check and died inside
ListenAndServe with EADDRNOTAVAIL, several seconds later, in a message that
mentioned neither Tailscale nor the reason.

### Acting on a red line

A health tab that can only tell you something is broken is a page you read on
your phone and then act on somewhere else, and the gap between those two is
where things stay broken. So a finding can be handed to the org: it is graded,
the chain is laid out, and the first role opens as a session you can take over,
in the directory the automation actually lives in.

Where that is comes from the registry, declared alongside the cadence, never
inferred from the name. An agent sent to fix a pipeline in the wrong tree is
worse than no agent. Two of the ten have no home and say so rather than opening
something to have an answer: `job-discovery` runs on Railway and
`machine-pressure` is a reading, not a program.

The brief is built on the server from what the sweep actually saw, not from a
fresh probe. Those differ exactly when the failure is intermittent, which is
when the brief matters most. It states the verdict and stops; telling an agent
what is probably wrong hands it a conclusion, and a wrong conclusion stated with
authority costs more than none, because it will spend its context confirming the
guess instead of reading the log.

Not everything wants an agent. Half of these are a shell script and a log file,
so the same row also opens a plain terminal there.

### Spend, read rather than recomputed

`amac cost` can only see sessions amac started, priced from whatever the adapter
reported, which for Codex is nothing. [looseapi](https://github.com/lgoyal6/LooseAPI)
reads the session logs both CLIs write whoever started them, and the billing mail
that is the only source in existence for a credit balance falling to zero. No
card statement can see that, because no transaction happens.

So the board reads looseapi's snapshot instead of growing a second answer.
Two implementations of "what am I spending" that can disagree is worse than one
that lives in another repo, and that one has more inputs. The snapshot is a fair
thing to read rather than a cache to distrust: it is written only after the mail
scan, the provider poll and the usage read have all completed, so a run that
died halfway leaves the previous one in place.

That artifact also upgraded the weakest probe in the health registry. `devspend`
used to be judged on its log's mtime, which cannot tell a finished run from one
that died after its first line. It now reads `generatedAt` out of the snapshot,
which is a real delivery marker. Finding the artifact was cheaper than adding
one.

It also carries the breakdown looseapi computes and nothing had ever read:
which projects the cost went to, and which models ran it. Merged across tools
rather than reported per tool, because the question is what a project cost and
not what it cost in Claude Code specifically; two agents working in the same
repo are one line of spend.

The agent figure keeps looseapi's label: equivalent API cost, not spend. On a
flat subscription those tokens cost nothing marginal, and quietly upgrading the
number into money on the way across a repo boundary is exactly the kind of
overstatement a cost report must never make.

### The org, from the board

`amac crew` lays a task out as a chain of roles and opens each as a session you
can take over. The board is a second front end to that same mechanism, not a
second mechanism: type a task, it is graded, the chain is shown with each role's
state, and one click opens the next role whose input exists.

Roles open one at a time, and a finished role's artifact is readable from the
same screen. Both follow from the handoff being a file: you can read the plan on
a phone and decide whether the executor should ever see it, which is the whole
reason a human is in this loop at all.

## A queue, and why it is not the log

The org is a chain: planner, then executor, then verifier, each waiting on the
one before. That is right when a task has stages and wrong when there are simply
several unrelated things to do, which is most days. So there is a queue agents
pull from, and the hard part is not the parallelism.

**A log is the wrong structure for mutual exclusion.** Everything else here is a
view over the append-only log, and this deliberately is not. Deciding whether a
task is free by reading the log means reading every claim and release ever
written and hoping nobody appended between the read and the write. Two agents
asking at the same moment both see it free, both append a claim, and the log
faithfully records that the work was done twice. A claim is a conditional
`UPDATE` in a transaction, which SQLite serialises across processes; the log
keeps the history. The log answers "what happened", the table answers "who holds
this right now", and only the second one has to be atomic.

**Leases create the zombie problem, and only a fence closes it.** A worker that
dies must not hold a task forever, so a claim expires. That introduces the
failure every lease scheme has: worker A stalls, its lease lapses, B takes the
task, and A wakes up and reports a result for work B is now doing. A lease alone
cannot prevent this, because A has no way to know it was declared dead. Every
claim carries a fencing token that only goes up, and a result is accepted only
when its token matches the claim the table currently holds, so a revived
worker's write is rejected on arrival rather than trusted because it arrived.

Proved by causing it rather than by asserting it. Children claim five tasks
each, are SIGKILLed holding them, and the survivor drains what they abandoned:

```
120 tasks, 6 kills holding 5 each, 150 attempts, 120 finished exactly once, 0 duplicated
```

150 attempts over 120 tasks is every one of the thirty abandoned claims
recovered, which is the number that says the crash path was actually exercised
rather than stepped around. Sixteen workers racing over 200 tasks produce zero
overlapping claims.

```
amac task add <title>            file work
amac task claim [-open]          take the next one, optionally opening a session
amac task done -token N <id>     finish it, if you still hold it
```

That last flag is the whole mechanism showing through. The token is printed
because every later call needs it, and a worker that has been fenced is told so
rather than having its result quietly dropped.

### The bug this found

SQLite pragmas are per-connection and `database/sql` opens connections on
demand, so `db.Exec("PRAGMA busy_timeout=5000")` configures whichever single
connection happened to serve it and leaves every later one on the default, which
is zero: fail instantly. The log had carried a comment since it was written
explaining why that timeout matters, and had not actually been granting it. It
never showed because amac had one writer per process until sixteen of them
raced. The pragmas are in the DSN now, where they apply to every connection the
pool opens.

## Measuring the router

The roadmap says the evaluation harness lands *before* the router is trusted,
because the measured cost/quality curve is the only claim worth making. The
harness is here. The curve is not: that needs a key and real models, and what
follows was produced against stub models to exercise the machinery.

What changed is what the harness is allowed to know.

**The cascade must not be gated on the answer key.** `amac eval` handed the
router each task's own check as its verifier. For `one_of` and `json_keys` that
is fair, because production genuinely knows the label set and the required keys
before the call. For `contains` and `regex` the check *carries the answer*, so
the cascade was rejecting cheap answers it could only tell were wrong because it
had been shown the right one. Nothing in production can do that, so every routed
number was a property of the harness rather than of the router.

Same stub models, same eight tasks, the only difference being what the cascade
was allowed to see:

| gate | routed quality | cost vs strong | escalations |
| --- | --- | --- | --- |
| the task's own check | 100.0% | -32% | 4 |
| what production can check | 75.0% | -40% | 2 |

The old harness overstated quality by 25 points *and* understated the saving,
because two of its four escalations were impossible. The real trade on this
suite is cheaper and worse. That is a statement worth making; "routing is free"
is not, and it is exactly the quality loss you never find out about that the
cascade exists to prevent.

So the report states the census as well, because it bounds the claim:

```
routed gating: 4 of 8 tasks gated as production would; 4 carry their
answer in the check and fell back to non-empty output.
```

Each run appends `eval.completed` carrying the arms, that census, and the model
behind every tier. A curve you cannot attribute to specific models is a number
you cannot re-check once they change underneath you.

## Roadmap

Session babysitting is **out of scope**. Claude Code Remote Control now pushes
to the phone when a task finishes or needs a decision, takes the approval from
there, and suppresses on terminal *focus* rather than mere presence, which is
the bug that made the tmux predecessor useless in practice. Rebuilding that
would be rebuilding a feature the vendor ships. What is left is what Remote
Control does not reach: Codex and Gemini sessions, automation health, and cost
across all of them.

1. ~~**Daemon**: supervise sessions, API, dashboard on the tailnet~~. Shipped,
   including the sessions amac did not start, a mirrored pane you can type
   into, and the org run from the same page. SSE rather than WebSocket: it
   carries an event id and browsers replay from it, so reconnect-with-replay is
   the protocol's default instead of something to hand-roll
2. **Gateway and router**: the gateway (any OpenAI-compatible host, one key
   for all three tiers) and the cascade are in, and the harness that has to
   justify them now measures the cascade without showing it the answer key.
   What is left is the curve itself, against real models rather than stubs
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
