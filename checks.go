package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
)

// Process states as printed
const (
	stateRunning = "running"
	stateStopped = "stopped"
	stateUnknown = "unknown"

	// Exit status of the Linux script when pgrep is unusable
	unavailableStatus = 3
	// Room for the remote check beyond the SSH connect deadline
	checkGrace = 5 * time.Second
)

// Outcome for one process on one host
type ProcessStatus struct {
	Host    RemoteHost
	Process string
	State   string
	Reason  string
}

// Prints 1 or 0 per process; exit 3 when pgrep is unusable
const linuxScript = `command -v pgrep >/dev/null 2>&1 || exit 3
for process_name do
    pgrep -x -- "$process_name" >/dev/null 2>&1
    result=$?
    if [ "$result" -eq 0 ]; then
        printf '1\n'
    elif [ "$result" -eq 1 ]; then
        printf '0\n'
    else
        exit 3
    fi
done`

// Prints 1 or 0 per process; names match with or without .exe
const windowsScript = `$ErrorActionPreference = 'Stop'
$targets = ConvertFrom-Json -InputObject '%s'
$running = @(Get-Process | ForEach-Object { $_.ProcessName })
foreach ($target in $targets) {
    $hasExeSuffix = $target.EndsWith(
        '.exe',
        [StringComparison]::OrdinalIgnoreCase
    )
    $normalized = if ($hasExeSuffix) {
        $target.Substring(0, $target.Length - 4)
    } else {
        $target
    }
    if ($running -contains $normalized) {
        [Console]::Out.WriteLine('1')
    } else {
        [Console]::Out.WriteLine('0')
    }
}`

var shellSafe = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

// POSIX shell quoting for one argument
func shellQuote(argument string) string {
	if shellSafe.MatchString(argument) {
		return argument
	}
	return "'" + strings.ReplaceAll(argument, "'", `'"'"'`) + "'"
}

func linuxCommand(processes []string) string {
	arguments := []string{"sh", "-c", shellQuote(linuxScript), projectName}
	for _, process := range processes {
		arguments = append(arguments, shellQuote(process))
	}
	return strings.Join(arguments, " ")
}

// PowerShell script passed as UTF-16LE base64
func windowsCommand(processes []string) string {
	targets, _ := json.Marshal(processes)
	script := fmt.Sprintf(windowsScript, strings.ReplaceAll(string(targets), "'", "''"))
	var encoded bytes.Buffer
	binary.Write(&encoded, binary.LittleEndian, utf16.Encode([]rune(script)))
	return "powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand " +
		base64.StdEncoding.EncodeToString(encoded.Bytes())
}

// Check all hosts with bounded concurrency; order is preserved
func runChecks(ctx context.Context, config Config) []ProcessStatus {
	limit := make(chan struct{}, config.Settings.Concurrency)
	groups := make([][]ProcessStatus, len(config.Hosts))
	var group sync.WaitGroup
	for index, host := range config.Hosts {
		group.Go(func() {
			groups[index] = unknownResults(host, "cancelled")
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
				groups[index] = checkHost(ctx, host, config.Settings.Timeout)
			case <-ctx.Done():
			}
		})
	}
	group.Wait()
	var results []ProcessStatus
	for _, statuses := range groups {
		results = append(results, statuses...)
	}
	return results
}

// One SSH connection per host covering all its processes
func checkHost(ctx context.Context, host RemoteHost, timeout time.Duration) []ProcessStatus {
	destination := host.Address
	if host.SSHUser != "" {
		destination = host.SSHUser + "@" + host.Address
	}
	remote := linuxCommand(host.Processes)
	if host.OS == osWindows {
		remote = windowsCommand(host.Processes)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout+checkGrace)
	defer cancel()
	command := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout="+strconv.Itoa(max(1, int(math.Ceil(timeout.Seconds())))),
		"-o", "ConnectionAttempts=1",
		"-o", "StrictHostKeyChecking=yes",
		"-p", strconv.Itoa(host.SSHPort),
		"--", destination, remote)
	command.Env = append(os.Environ(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	// Graceful stop first, kill after the delay
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 500 * time.Millisecond
	err := command.Run()

	var exit *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return unknownResults(host, "timeout")
	case ctx.Err() != nil:
		return unknownResults(host, "cancelled")
	case errors.Is(err, exec.ErrNotFound):
		return unknownResults(host, "missing local dependency: ssh")
	case errors.As(err, &exit) && exit.ExitCode() == unavailableStatus:
		return unknownResults(host, "missing remote dependency: pgrep")
	case errors.As(err, &exit):
		return unknownResults(host, sshFailure(stderr.String(), exit.ExitCode()))
	case err != nil:
		return unknownResults(host, "check failed: "+strings.Join(strings.Fields(err.Error()), " "))
	}

	output := strings.Split(strings.TrimSuffix(strings.ReplaceAll(stdout.String(), "\r\n", "\n"), "\n"), "\n")
	if len(output) != len(host.Processes) {
		return unknownResults(host, "invalid remote output")
	}
	results := make([]ProcessStatus, len(output))
	for index, value := range output {
		results[index] = ProcessStatus{Host: host, Process: host.Processes[index]}
		switch value {
		case "1":
			results[index].State = stateRunning
		case "0":
			results[index].State = stateStopped
		default:
			return unknownResults(host, "invalid remote output")
		}
	}
	return results
}

func unknownResults(host RemoteHost, reason string) []ProcessStatus {
	results := make([]ProcessStatus, len(host.Processes))
	for index, process := range host.Processes {
		results[index] = ProcessStatus{host, process, stateUnknown, reason}
	}
	return results
}

// Short, stable reason from the SSH client's stderr
func sshFailure(stderr string, code int) string {
	known := [][2]string{
		{"Host key verification failed", "host key not trusted"},
		{"REMOTE HOST IDENTIFICATION HAS CHANGED", "host key not trusted"},
		{"Permission denied", "authentication failed"},
		{"Could not resolve hostname", "unknown host"},
		{"Connection refused", "connection refused"},
		{"timed out", "timeout"},
		{"No route to host", "host unreachable"},
		{"Network is unreachable", "host unreachable"},
	}
	for _, entry := range known {
		if strings.Contains(stderr, entry[0]) {
			return entry[1]
		}
	}
	// Otherwise the last line the client or remote shell printed
	lines := strings.Split(stderr, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if line := strings.Join(strings.Fields(lines[index]), " "); line != "" {
			return line
		}
	}
	return fmt.Sprintf("ssh exited with status %d", code)
}
