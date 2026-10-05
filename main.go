// processprobe checks configured processes on Linux and Windows hosts over SSH.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

const help = appName + ` — check processes on Linux and Windows hosts over SSH

Usage:
  ` + appName + ` [--config PATH]
  ` + appName + ` --help | -h

Options:
  --config PATH  Exact YAML configuration path; no fallback

Configuration:
  Without --config, uses the first existing file without merging:
  1. config.yml in the current working directory
  2. ~/.config/` + appName + `/config.yml
  With no file, creates a user-only template at location 2 and exits 1.

Behavior:
  Opens one non-interactive SSH connection per host, checks every configured
  process with pgrep (Linux) or Get-Process (Windows), and writes each result
  to the PostgreSQL processes table. Missing rows are inserted. Anything other
  than confirmed running is stored as false.

Exit codes:
  0    Checks finished and all results were saved
  1    Configuration or database error
  2    Invalid command-line arguments
  130  Interrupted by the user
`

const (
	grey     = "1;90"
	lred     = "1;91"
	lgreen   = "1;92"
	lyellow  = "1;93"
	lblue    = "1;94"
	lmagenta = "1;95"
	lcyan    = "1;96"
	white    = "1;97"
)

// Colored output for one stream
type printer struct {
	out   io.Writer
	color bool
}

// Colors only on a terminal, unless NO_COLOR or TERM=dumb
func newPrinter(file *os.File) printer {
	_, noColor := os.LookupEnv("NO_COLOR")
	return printer{file, !noColor && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(file.Fd()))}
}

func (p printer) colorize(text, color string) string {
	if !p.color {
		return text
	}
	return "\033[" + color + "m" + text + "\033[0m"
}

func (p printer) message(icon, text, color string) {
	fmt.Fprintln(p.out, p.colorize(icon+"  "+text, color))
}

// Framed startup banner
func (p printer) header() {
	title := "  ⚙️  " + appName + " — starting  "
	border := strings.Repeat("─", utf8.RuneCountInString(title))
	fmt.Fprintln(p.out)
	for _, line := range []string{"╭" + border + "╮", "│" + title + "│", "╰" + border + "╯"} {
		fmt.Fprintln(p.out, p.colorize(line, lcyan))
	}
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	stderr := newPrinter(os.Stderr)
	flags := flag.NewFlagSet(appName, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "")
	err := flags.Parse(arguments)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(help)
		return 0
	}
	if err == nil && flags.NArg() > 0 {
		err = fmt.Errorf("unrecognized arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err != nil {
		stderr.message("❌", err.Error(), lred)
		fmt.Fprintf(os.Stderr, "Run '%s --help' for usage.\n", appName)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err = probe(ctx, newPrinter(os.Stdout), *configPath)
	if ctx.Err() != nil {
		stderr.message("🛑", "Interrupted; running checks cancelled", lyellow)
		return 130
	}
	if err != nil {
		stderr.message("❌", err.Error(), lred)
		return 1
	}
	return 0
}

// Load, check, report, persist
func probe(ctx context.Context, out printer, configPath string) error {
	started := time.Now()
	out.header()
	path, config, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	out.message("📂", "Loaded configuration from "+path, lblue)
	printCheckPlan(out, config)
	out.message("🚀", "Checking remote processes…", lcyan)
	results := runChecks(ctx, config)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	fmt.Fprintln(out.out)
	out.message("📋", "Process results", white)
	printResults(out, results)

	fmt.Fprintln(out.out)
	database := config.Database
	out.message("💾", fmt.Sprintf("Updating PostgreSQL at %s:%d /%s…", database.Host, database.Port, database.Name), lblue)
	updated, inserted, err := updateStatuses(ctx, database, results)
	if err != nil {
		return err
	}
	out.message("✅", fmt.Sprintf("Persisted %d process statuses (%d updated, %d inserted)",
		updated+inserted, updated, inserted), lgreen)
	printSummary(out, results, time.Since(started))
	return nil
}

func printCheckPlan(out printer, config Config) {
	processes := 0
	for _, host := range config.Hosts {
		processes += len(host.Processes)
	}
	out.message("⚙️", "Configuration loaded", lgreen)
	out.message("🔐", fmt.Sprintf("SSH: %d hosts, %d processes, concurrency %d, timeout %ds",
		len(config.Hosts), processes, config.Settings.Concurrency, config.Settings.TimeoutSeconds), lmagenta)
	validation := "disabled"
	if len(config.Settings.AllowedNetworks) > 0 {
		validation = joinNetworks(config.Settings.AllowedNetworks)
	}
	out.message("🛡️", "Network validation: "+validation, lcyan)
}

func printResults(out printer, results []ProcessStatus) {
	for _, result := range results {
		icon, color, label := "❔", lyellow, "unknown"
		switch result.State {
		case stateRunning:
			icon, color, label = "✅", lgreen, "running"
		case stateStopped:
			icon, color, label = "❌", lred, "not running"
		}
		metadata := fmt.Sprintf("(%.2fs)", result.Duration.Seconds())
		if result.Detail != "" {
			metadata = fmt.Sprintf("(%.2fs, %s)", result.Duration.Seconds(), result.Detail)
		}
		fmt.Fprintf(out.out, "  %s %s: %s %s\n", icon,
			out.colorize(result.Host.Name+"/"+result.Process, white),
			out.colorize(label, color), out.colorize(metadata, grey))
	}
}

func printSummary(out printer, results []ProcessStatus, duration time.Duration) {
	counts := map[state]int{}
	for _, result := range results {
		counts[result.State]++
	}
	icon, color := "🎉", lgreen
	if counts[stateStopped]+counts[stateUnknown] > 0 {
		icon, color = "⚠️", lyellow
	}
	fmt.Fprintln(out.out)
	out.message(icon, fmt.Sprintf("Completed in %.2fs — %d running, %d stopped, %d unknown, %d checked",
		duration.Seconds(), counts[stateRunning], counts[stateStopped], counts[stateUnknown], len(results)), color)
}
