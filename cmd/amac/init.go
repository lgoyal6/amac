package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lgoyal6/amac/internal/health"
)

// exampleRoster is what a fresh install gets.
//
// Three entries rather than one, because the interesting thing about the roster
// is not the syntax but the idea that cadence and grace are declared: an
// automation that does not say how often it should deliver can go dark forever
// without anyone noticing, and that is the failure this whole subsystem exists
// to catch. One of each shape that most people will actually have.
//
// JSON has no comments, so the file is deliberately readable rather than
// exhaustive, and the README carries the rest.
const exampleRoster = `{
  "automations": [
    {
      "name": "nightly-backup",
      "what": "example: a launchd job that appends a completion marker to its log",
      "every": "24h",
      "grace": "4h",
      "home": "~/scripts",
      "probe": "launchd_marker",
      "with": {
        "label": "com.example.backup",
        "log": "~/Library/Logs/backup.log"
      }
    },
    {
      "name": "amac-daemon",
      "what": "the board, served on the tailnet",
      "home": "~/amac",
      "probe": "service",
      "with": { "label": "com.amac.daemon", "port": 7788 }
    },
    {
      "name": "example-pipeline",
      "what": "example: a GitHub Actions pipeline that commits a delivery marker",
      "every": "24h",
      "grace": "8h",
      "probe": "github_delivery_file",
      "with": {
        "repo": "you/your-repo",
        "path": "out/.delivery.json",
        "date_field": "scheduled_date",
        "anchor_hour_utc": 16
      }
    }
  ]
}
`

// cmdInit writes a starter roster.
//
// It refuses to overwrite. A roster is hand-edited and is the only record of
// what someone expects their machine to be doing; clobbering it because a
// command was run twice would delete the thing amac is meant to protect.
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := fs.String("config", health.ConfigPath(), "where to write the roster")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if _, err := os.Stat(*path); err == nil {
		fmt.Printf("%s already exists, leaving it alone\n", *path)
		return checkRoster(*path)
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*path, []byte(exampleRoster), 0o644); err != nil {
		return err
	}

	fmt.Printf("wrote %s\n\n", *path)
	fmt.Printf("It declares three example automations. Edit it to name yours, then\n")
	fmt.Printf("`amac health` to check them. The two fields worth thinking about are\n")
	fmt.Printf("`every` and `grace`: they are what turn \"I heard nothing\" into a\n")
	fmt.Printf("finding instead of into silence.\n\n")
	return checkRoster(*path)
}

// checkRoster loads the file and says what it found, so a typo surfaces now
// rather than on the next unattended sweep.
func checkRoster(path string) error {
	list, err := health.Load(path)
	if err != nil {
		return err
	}
	fmt.Printf("%d automation(s) declared:\n", len(list))
	for _, a := range list {
		cadence := "no cadence (liveness)"
		if a.Every > 0 {
			cadence = "every " + a.Every.String()
		}
		fmt.Printf("  %-24s %s\n", a.Name, cadence)
	}
	return nil
}
