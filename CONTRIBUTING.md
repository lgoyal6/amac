# Contributing

Thanks for looking. A few things worth knowing before you spend time.

## What this runs on

macOS only, and not incidentally. amac reads launchd for local job state,
resolves the frontmost terminal through the window server to decide whether to
interrupt you, and reads the login keychain for credentials. Those are three
different macOS-specific things and none of them has a portable equivalent that
would mean the same. A Linux port is welcome but it is a port, not a flag.

You also need `tmux`, Go 1.26+, and Tailscale if you want the board on a phone.
Discord is optional and everything works without it.

## Getting it running

```bash
go build -o bin/amac ./cmd/amac
bin/amac init          # writes a health roster you then edit
bin/amac hooks         # shows which agent signals reach amac
go test ./...
```

`amac init` writes example automations, not real ones. Editing that file to
name your own is the setup step, and it is deliberately manual: guessing which
of your jobs matter would produce a roster full of things you did not ask to be
woken up about.

## What the tests do

Some of them fork a child process and SIGKILL it. `TestCrashDurability` proves
the event log's durability guarantee that way, and the queue tests prove that
work held by a killed worker is recovered exactly once. They are slower than
unit tests and they are the ones worth keeping green.

CI runs on macOS, and fails on unformatted code, on `go vet`, and on any
unreachable function. That last one is deliberate: dead code here has always
turned out to be a feature that was half-removed.

## The one rule

**Do not report a state you have not established.**

Most of this codebase is downstream of that. A probe that cannot reach its
target reports `unknown`, never `ok`. A monitor counts deliveries, not runs,
because most runs here deliberately do nothing. Session state comes from the
agent's own hooks and never from parsing a rendered terminal, which the
predecessor did and got confidently wrong often enough to need two competing
detectors.

If you are adding something that reports a status, the question to answer in the
comment is what it does when it cannot tell. "It shows green" is the wrong
answer and is how a monitor becomes worse than no monitor.

## Style

Comments explain why, not what. If a line needs a comment saying what it does,
the line is usually the problem. Commit messages state the failure being fixed
and what was considered instead; the git log here is meant to be readable as a
record of decisions.

## Filing something

An issue describing what you expected, what happened, and what you had to do to
find out is worth more than a patch. If it is a wrong status, include the output
and what the true state was, because those are the bugs this project cares about
most.
