// Shared CLI frame: help, colored output, signals, exit codes.
// Copy unchanged; the app supplies projectName, help, and execute.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// ANSI colors of the shared output language
const (
	red    = "31"
	green  = "32"
	yellow = "33"
	cyan   = "36"
)

// Colored line output
type printer struct {
	out   io.Writer
	color bool
}

// Framed startup banner
func (p printer) header() {
	title := "  ⚙️  " + projectName + " — starting  "
	border := strings.Repeat("─", utf8.RuneCountInString(title))
	fmt.Fprintln(p.out)
	for _, line := range []string{"╭" + border + "╮", "│" + title + "│", "╰" + border + "╯"} {
		p.print(line, cyan)
	}
}

func (p printer) line(icon, text, color string) {
	p.print(icon+" "+text, color)
}

// Categorized error line; returns the failure exit code
func (p printer) fail(category string, err error) int {
	p.line("❌", fmt.Sprintf("%s error: %v", category, err), red)
	return 1
}

func (p printer) print(text, color string) {
	if p.color && color != "" {
		text = "\033[" + color + "m" + text + "\033[0m"
	}
	fmt.Fprintln(p.out, text)
}

// Help, argument check, colors, signals, exit code
func run(arguments []string) int {
	if len(arguments) == 1 && (arguments[0] == "-h" || arguments[0] == "--help" || arguments[0] == "help") {
		fmt.Print(help)
		return 0
	}
	if len(arguments) > 0 {
		fmt.Fprintf(os.Stderr, "❌ Unknown argument: %s\n", arguments[0])
		fmt.Fprintf(os.Stderr, "Run '%s --help' for usage.\n", projectName)
		return 1
	}

	_, noColor := os.LookupEnv("NO_COLOR")
	out := printer{os.Stdout, term.IsTerminal(int(os.Stdout.Fd())) && !noColor}

	// First Ctrl-C cancels cleanly, second one kills
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	context.AfterFunc(ctx, stop)

	code := start(ctx, out)
	if ctx.Err() != nil {
		out.line("🛑", "Interrupted", yellow)
		return 130
	}
	return code
}

// Banner and configuration, then the app's own work
func start(ctx context.Context, out printer) int {
	out.header()
	binaryDir, err := executableDir()
	if err != nil {
		return out.fail("Configuration", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return out.fail("Configuration", err)
	}
	_, config, err := loadConfig(binaryDir, home)
	var created *configCreatedError
	if errors.As(err, &created) {
		out.line("📝", "Created configuration: "+created.path, yellow)
		out.line("✏️", "Replace every "+placeholder+" value, then rerun", "")
		return 1
	}
	if err != nil {
		return out.fail("Configuration", err)
	}
	return execute(ctx, out, config)
}
