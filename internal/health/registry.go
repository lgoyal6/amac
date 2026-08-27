package health

import "time"

// All is the declared set. Every and Grace are the contract: they are what
// turns "I heard nothing" into a finding instead of into silence.
//
// The cadences are the delivery cadence, not the cron cadence. Each of these
// pipelines fires several crons per delivery on purpose, so the crons say
// nothing about how often work is supposed to land.
func All() []Automation {
	return []Automation{
		{
			Name:  "morning-brief",
			What:  "daily AI/markets/geopolitics brief, DM'd as a PDF",
			Every: 24 * time.Hour,
			// Four crons spread over two hours, then Pacific/UTC drift, then
			// GitHub's own scheduling lateness. Under six hours of grace this
			// pages on a normal DST-boundary morning.
			Grace: 8 * time.Hour,
			Check: MorningBrief,
		},
		{
			Name:  "hacklist-sf",
			What:  "SF hackathon discovery sweep, published as a calendar feed",
			Every: 12 * time.Hour, // 8am and 8pm Pacific
			Grace: 6 * time.Hour,
			Check: Hacklist,
		},
		{
			Name:  "job-discovery",
			What:  "two-hourly job digest (n8n on Railway)",
			Every: 2 * time.Hour,
			// One skipped cycle is noise; two in a row is a problem.
			Grace: 2 * time.Hour,
			Check: JobDiscovery,
		},
		{
			Name:  "hacklist-local-passes",
			What:  "nightly local Luma pass writer (launchd, 20:30)",
			Every: 24 * time.Hour,
			Grace: 4 * time.Hour,
			Check: LocalPasses,
		},
		{
			Name:  "brew-autoupgrade",
			What:  "daily Homebrew, npm global and pipx upgrade (launchd, 09:30)",
			Every: 24 * time.Hour,
			// Only fires while the Mac is awake, so a closed lid over a weekend
			// legitimately skips a day. Two missed days is the real signal.
			Grace: 24 * time.Hour,
			Check: BrewUpgrade,
		},
		{
			Name:  "tmux-idle-reaper",
			What:  "kills detached agent sessions idle over 8h (launchd, every 30m)",
			Every: 30 * time.Minute,
			// One skipped tick is a laptop that slept. Two is the prevention
			// having stopped, and a prevention that has stopped preventing is
			// invisible by construction: the symptom is sessions accumulating
			// over days, not an error anywhere.
			Grace: 30 * time.Minute,
			Check: TmuxReaper,
		},
		{
			Name:  "disk-sweep",
			What:  "weekly clean of regenerable caches (launchd, Sun 11:00)",
			Every: 7 * 24 * time.Hour,
			// A Sunday with the lid shut pushes it to Monday. Missing two
			// Sundays is the signal, and on a weekly cadence there is no rush.
			Grace: 48 * time.Hour,
			Check: DiskSweep,
		},
		{
			Name:  "devspend",
			What:  "daily spend digest (launchd, 09:15)",
			Every: 24 * time.Hour,
			Grace: 24 * time.Hour,
			Check: DevSpend,
		},
		{
			Name: "amac-daemon",
			What: "the board, served on the tailnet (launchd, always on)",
			// A service, not a schedule. Its probe proves liveness directly and
			// leaves Last zero, which suppresses the cadence test. Declaring a
			// fake cadence here would be declaring a fake delivery: the honest
			// statement is that "when did it last deliver" is the wrong
			// question about something that is either up or down right now.
			Every: 0,
			Grace: 0,
			Check: AmacDaemon,
		},
	}
}
