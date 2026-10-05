// processprobe checks configured processes on Linux and Windows hosts over SSH once.
package main

import (
	"context"
	"fmt"
	"os"
)

const projectName = "processprobe"

const help = projectName + ` — check processes on Linux and Windows hosts over SSH

Usage:
  ` + projectName + `
  ` + projectName + ` --help | -h | help

Configuration:
  Uses the first existing file without merging:
  1. config.yml beside the executable
  2. ~/.config/` + projectName + `/config.yml
  The working directory is not searched.
  With no file, creates a user-only template at location 2 and exits 1.

Behavior:
  Opens one non-interactive SSH connection per host with strict host keys and
  checks every configured process with pgrep (Linux) or Get-Process (Windows).
  All results are written to the PostgreSQL processes table in one
  transaction; missing rows are inserted. Anything other than confirmed
  running is stored as false.

Exit codes:
  0    Every process was checked and saved; stopped processes are not errors
  1    Configuration, dependency, database, or unreachable-host error
  130  Interrupted by the user
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// Check, report, save
func execute(ctx context.Context, out printer, config Config) int {
	processes := 0
	for _, host := range config.Hosts {
		processes += len(host.Processes)
	}
	out.line("🔎", fmt.Sprintf("Checking %d processes on %d hosts", processes, len(config.Hosts)), cyan)
	results := runChecks(ctx, config)
	if ctx.Err() != nil {
		return 130
	}

	failed := false
	for _, result := range results {
		text := result.Host.Name + " · " + result.Process + " · " + result.State
		switch result.State {
		case stateRunning:
			out.line("✅", text, green)
		case stateStopped:
			out.line("❌", text, red)
		default:
			failed = true
			out.line("⚠️", text+" · "+result.Reason, yellow)
		}
	}

	updated, inserted, err := saveStatuses(ctx, config.Database, results)
	if ctx.Err() != nil {
		return 130
	}
	if err != nil {
		return out.fail("Database", err)
	}
	out.line("✅", fmt.Sprintf("Saved %d statuses · %d updated · %d inserted", updated+inserted, updated, inserted), green)
	if failed {
		return 1
	}
	return 0
}
