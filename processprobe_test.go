package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const validConfig = `database:
  host: "db.example"
  port: 5432
  name: "monitoring"
  user: "monitoring"
  password: "secret"
settings:
  ssh_concurrency: 4
  ssh_timeout_seconds: 2.5
  allowed_networks: ["192.0.2.0/24"]
hosts:
  - name: "linux-01"
    address: "192.0.2.10"
    os: "linux"
    ssh_user: "automation"
    processes: ["nginx", "postgres"]
  - name: "windows-01"
    address: "win.example"
    os: "Windows"
    ssh_port: 2222
    processes: ["spoolsv.exe"]
`

func parseYAML(t *testing.T, text string) (Config, error) {
	t.Helper()
	var document any
	if err := yaml.Unmarshal([]byte(text), &document); err != nil {
		t.Fatal(err)
	}
	return parseConfig(document)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Fake ssh client running a fixed script
func fakeSSH(t *testing.T, script string) {
	t.Helper()
	directory := t.TempDir()
	writeFile(t, filepath.Join(directory, "ssh"), "#!/bin/sh\n"+script+"\n")
	if err := os.Chmod(filepath.Join(directory, "ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
}

func TestExecutableConfigTakesPrecedence(t *testing.T) {
	binaryDir, home := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(binaryDir, "config.yml"), validConfig)
	writeFile(t, filepath.Join(home, ".config", projectName, "config.yml"), "not: valid")

	path, config, err := loadConfig(binaryDir, home)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(binaryDir, "config.yml") || config.Settings.Timeout != 2500*time.Millisecond {
		t.Fatalf("unexpected selection: %s %+v", path, config.Settings)
	}
	linux, windows := config.Hosts[0], config.Hosts[1]
	if linux.OS != osLinux || linux.SSHPort != 22 || linux.SSHUser != "automation" {
		t.Fatalf("unexpected linux host: %+v", linux)
	}
	if windows.SSHPort != 2222 || windows.SSHUser != "" {
		t.Fatalf("unexpected windows host: %+v", windows)
	}
}

func TestInvalidConfigDoesNotFallBackOrLeak(t *testing.T) {
	binaryDir, home := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(binaryDir, "config.yml"), "database: [secret-content\n")
	writeFile(t, filepath.Join(home, ".config", projectName, "config.yml"), validConfig)

	_, _, err := loadConfig(binaryDir, home)
	if err == nil || strings.Contains(err.Error(), "secret-content") || !strings.Contains(err.Error(), binaryDir) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFirstRunCreatesTemplate(t *testing.T) {
	binaryDir, home := t.TempDir(), t.TempDir()
	_, _, err := loadConfig(binaryDir, home)
	var created *configCreatedError
	if !errors.As(err, &created) {
		t.Fatalf("expected a created template, got %v", err)
	}
	info, err := os.Stat(created.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected template: %v %v", info, err)
	}
	if _, _, err := loadConfig(binaryDir, home); err == nil || !strings.Contains(err.Error(), "edit placeholder value at database.password") {
		t.Fatalf("unedited template accepted: %v", err)
	}

	example, err := os.ReadFile("config.yml.example")
	if err != nil {
		t.Fatal(err)
	}
	var fromExample, fromDefault any
	if yaml.Unmarshal(example, &fromExample) != nil || yaml.Unmarshal([]byte(defaultConfig), &fromDefault) != nil ||
		fmt.Sprint(fromExample) != fmt.Sprint(fromDefault) {
		t.Fatal("config.yml.example differs from the embedded template")
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string][2]string{
		"placeholder":      {`password: "secret"`, `password: "CHANGE_ME"`},
		"unknown key":      {`settings:`, "settings:\n  retries: 3"},
		"missing key":      {"  ssh_concurrency: 4\n", ""},
		"zero timeout":     {`ssh_timeout_seconds: 2.5`, `ssh_timeout_seconds: 0`},
		"bad network":      {`"192.0.2.0/24"`, `"192.0.2.0/40"`},
		"outside network":  {`"192.0.2.10"`, `"198.51.100.10"`},
		"unknown os":       {`os: "linux"`, `os: "plan9"`},
		"option address":   {`"win.example"`, `"-oProxyCommand=x"`},
		"duplicate":        {`["nginx", "postgres"]`, `["nginx", "nginx"]`},
		"bad port":         {`ssh_port: 2222`, `ssh_port: 70000`},
		"no processes":     {`["spoolsv.exe"]`, `[]`},
		"unknown host key": {`ssh_port: 2222`, "ssh_port: 2222\n    extra: 1"},
		"no hosts":         {validConfig[strings.Index(validConfig, "hosts:"):], "hosts: []\n"},
	}
	for name, replace := range cases {
		if !strings.Contains(validConfig, replace[0]) {
			t.Fatalf("%s: base text not found", name)
		}
		if _, err := parseYAML(t, strings.Replace(validConfig, replace[0], replace[1], 1)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRemoteCommands(t *testing.T) {
	linux := linuxCommand([]string{"nginx", "it's"})
	if !strings.HasSuffix(linux, ` processprobe nginx 'it'"'"'s'`) {
		t.Fatalf("unexpected quoting: %s", linux)
	}

	windows := windowsCommand([]string{"spoolsv.exe"})
	encoded := windows[strings.LastIndex(windows, " ")+1:]
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	// ASCII script, so every second byte is zero
	script := strings.ReplaceAll(string(raw), "\x00", "")
	if !strings.Contains(script, `'["spoolsv.exe"]'`) {
		t.Fatalf("unexpected script: %s", script)
	}
}

func TestCheckHost(t *testing.T) {
	host := RemoteHost{Name: "linux-01", Address: "192.0.2.10", OS: osLinux, Processes: []string{"nginx", "postgres"}, SSHPort: 22}

	fakeSSH(t, `printf '1\n0\n'`)
	results := checkHost(context.Background(), host, time.Second)
	if results[0].State != stateRunning || results[1].State != stateStopped {
		t.Fatalf("unexpected results: %+v", results)
	}

	reasons := map[string]string{
		`echo 'user@host: Permission denied (publickey).' >&2; exit 255`: "authentication failed",
		`echo 'Host key verification failed.' >&2; exit 255`:             "host key not trusted",
		`echo 'sh: 1: something odd' >&2; exit 1`:                        "sh: 1: something odd",
		`exit 3`: "missing remote dependency: pgrep",
		`echo 1`: "invalid remote output",
	}
	for script, reason := range reasons {
		fakeSSH(t, script)
		results = checkHost(context.Background(), host, time.Second)
		if results[0].State != stateUnknown || results[1].Reason != reason {
			t.Errorf("%s: unexpected results: %+v", script, results)
		}
	}

	// No ssh client at all
	t.Setenv("PATH", t.TempDir())
	if results = checkHost(context.Background(), host, time.Second); results[0].Reason != "missing local dependency: ssh" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestStalledHostFailsWithinDeadline(t *testing.T) {
	host := RemoteHost{Name: "linux-01", Address: "192.0.2.10", OS: osLinux, Processes: []string{"nginx"}, SSHPort: 22}
	fakeSSH(t, `exec /bin/sleep 60`)
	started := time.Now()
	results := checkHost(context.Background(), host, 200*time.Millisecond-checkGrace)
	if time.Since(started) > 3*time.Second {
		t.Fatalf("deadline ignored: %v", time.Since(started))
	}
	if results[0].State != stateUnknown || results[0].Reason != "timeout" {
		t.Fatalf("unexpected results: %+v", results)
	}
}
