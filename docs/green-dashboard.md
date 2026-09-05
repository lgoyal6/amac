# Your green dashboard is lying to you

For a week, my monitoring said all fourteen of my automations were healthy.

In that same week they failed thirteen times.

Nothing was broken about the monitor. It was doing exactly what I built it to
do, and what almost every status page does: it read the newest piece of
evidence and reported what it found. The job that feeds my job search crashed
eight times in seven days. Every single time I looked, it was green, because by
the time I looked the next run had already succeeded.

That is not a bug you fix. It is a category of question you have to stop asking.

## Self-healing is what makes it invisible

The pipelines that do this are not badly written. They are the well-behaved
ones. They retry. They are scheduled more often than they strictly need to be,
so a failure gets absorbed before it matters. Cron fires again in three hours
whether or not you noticed anything.

Which means the interval between "it broke" and "it looks fine again" is
usually shorter than the interval between you opening the dashboard twice.

A status check samples. Failures that self-heal live in the gaps between
samples. You can increase the sampling rate forever and never fix this, because
the thing you are sampling is a state that is deliberately transient.

You will find out eventually. You find out the day it fails and *doesn't*
recover, and then you discover it had been failing for two months, and the
question you cannot answer is which of the outputs you trusted in that window
were real.

## Three rules that came out of it

I rebuilt the health layer around these. They are not clever. They are just the
three things I had wrong.

### Count deliveries, not runs

Most of these pipelines are over-scheduled on purpose. A job that runs every
three hours to catch something that appears twice a day is *supposed* to do
nothing most of the time, and it exits zero when it does nothing.

So "last run succeeded" is almost always true, and almost meaningless. It is
true for the run that delivered, and equally true for the six that correctly
did nothing, and equally true for the one that silently produced an empty file.

The signal is the artifact that only exists if work actually landed. A commit
containing the file. A row in a table. A message that was actually sent. Check
for the thing the job is *for*, not for the job's exit code.

Concretely: my morning brief workflow reports success whether it delivered or
found the day's slot already claimed, because the skip happens inside the steps
rather than as a skipped step. GitHub cannot tell those apart. The delivery
commit landing inside the run's own window can.

### Detect silence

Here is the failure nothing pushes an event for: the job that didn't run at all.

A crashed cron sends nothing. A launchd job whose plist got unloaded sends
nothing. A workflow disabled after sixty days of repository inactivity sends
nothing. Your monitor sees no failures, because it sees nothing, and no failures
looks exactly like everything is fine.

Every automation has to declare how often it is supposed to produce evidence,
plus a grace period. Then something applies that test centrally, on a schedule
of its own, and asks: has this delivered inside its window?

Centrally is load-bearing. If each probe implements its own lateness check, one
of them will forget, and the one that forgets will be the one that goes quiet.

### A failed probe reports `unknown`, never `ok`

When the check itself breaks, when the API times out or the token expires or the
log file moved, the honest answer is "I could not establish this."

It is very tempting to write that as a pass. The code is simpler, the dashboard
is greener, and nothing appears to be wrong.

A false all-clear is worse than having no monitor at all. With no monitor you
know you are not being told anything. With a broken monitor reporting green you
believe you are covered, and you stop checking manually.

## The half I was still missing

Those three rules made the *current* verdict trustworthy. They did nothing for
history, because they still answer a question about right now.

A pipeline that broke and recovered is, right now, fine. Every rule above agrees
it is fine. It *is* fine. And you still want to know it broke, because six
failures in a week means something is wrong with it even though every individual
failure was survived.

So: report every run exactly once, whatever happened after it. Not the newest
state, not a rollup, one record per run that never gets retracted by the run
that followed.

That is when my thirteen failures appeared. They had been in the log the whole
time. Nothing was reading them, because everything was reading the newest row.

The screen for it turned out to need one more thing. When I first shipped it,
every recovered pipeline led with its failures, so three problems I had already
fixed read as three ongoing problems. The number that settles it is how many
runs have succeeded *since* the last failure, and you cannot derive it from a
failure count: "eight failed" reads identically whether the last one was an hour
ago or the thing has been solid for two days.

So the line reads: `16 runs clean since the last failure 2d ago. 8 runs failed
before that.` Good news first, history behind it, both true.

## What this costs you

Almost nothing, and that is the annoying part. There was no missing
infrastructure. The runs were already recorded. The event log already existed.
Every failure was sitting in a table I had written myself and was not querying,
because I had built a screen that answered a different question and then treated
its answer as though it covered everything.

If you have a status page, go and ask it a question it is not built for: how
many times did this fail this week? Not is it up. How many times did it break
and quietly fix itself while you were not looking?

If your monitoring cannot answer that, it is not telling you your systems are
healthy. It is telling you they are healthy *right now*, which is a much smaller
claim than the green dot implies.

---

The implementation is in [amac](https://github.com/lgoyal6/amac), a control
plane for the coding agents and automations on one Mac. `amac demo` will show
you the screen with a seeded week, no setup.
