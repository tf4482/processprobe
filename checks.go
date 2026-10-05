package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// Observed process state
type state int

const (
	stateUnknown state = iota
	stateRunning
	stateStopped
)

// Outcome for one process on one host
type ProcessStatus struct {
	Host     RemoteHost
	Process  string
	State    state
	Duration time.Duration
	Detail   string
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
	arguments := []string{"sh", "-c", shellQuote(linuxScript), appName}
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
			groups[index] = checkHost(ctx, host, limit, config.Settings.TimeoutSeconds)
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
func checkHost(ctx context.Context, host RemoteHost, limit chan struct{}, timeoutSeconds int) []ProcessStatus {
	destination := host.Address
	if host.SSHUser != "" {
		destination = host.SSHUser + "@" + host.Address
	}
	remote := linuxCommand(host.Processes)
	if host.OS == "Windows" {
		remote = windowsCommand(host.Processes)
	}
	started := time.Now()
	limit <- struct{}{}
	defer func() { <-limit }()

	// Hard deadline beyond the SSH connect timeout
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds+5)*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout="+strconv.Itoa(timeoutSeconds),
		"-o", "ConnectionAttempts=1",
		"-o", "StrictHostKeyChecking=yes",
		"-p", strconv.Itoa(host.SSHPort),
		destination, remote)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	command.WaitDelay = time.Second
	err := command.Run()
	duration := time.Since(started)

	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return unknownResults(host, duration, "SSH process check timed out")
	case errors.As(err, &exit) && exit.ExitCode() == 3:
		return unknownResults(host, duration, "Remote process-check command is unavailable")
	case errors.As(err, &exit):
		return unknownResults(host, duration, sshFailureDetail(stderr.String(), exit.ExitCode()))
	case err != nil:
		return unknownResults(host, duration, fmt.Sprintf("SSH unavailable: %v", err))
	}

	output := strings.Split(strings.TrimSuffix(strings.ReplaceAll(stdout.String(), "\r\n", "\n"), "\n"), "\n")
	if len(output) != len(host.Processes) {
		return unknownResults(host, duration, "Remote process check returned invalid output")
	}
	results := make([]ProcessStatus, len(output))
	for index, value := range output {
		results[index] = ProcessStatus{Host: host, Process: host.Processes[index], Duration: duration}
		switch value {
		case "1":
			results[index].State = stateRunning
		case "0":
			results[index].State = stateStopped
		default:
			return unknownResults(host, duration, "Remote process check returned invalid output")
		}
	}
	return results
}

func unknownResults(host RemoteHost, duration time.Duration, detail string) []ProcessStatus {
	results := make([]ProcessStatus, len(host.Processes))
	for index, process := range host.Processes {
		results[index] = ProcessStatus{Host: host, Process: process, Duration: duration, Detail: detail}
	}
	return results
}

// Last non-empty stderr line, else the exit status
func sshFailureDetail(stderr string, code int) string {
	lines := strings.Split(stderr, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if line := strings.TrimSpace(lines[index]); line != "" {
			return line
		}
	}
	return fmt.Sprintf("ssh exited with status %d", code)
}
