package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const validConfig = `database:
  host: "db.example"
  port: 5432
  name: "monitoring"
  user: "monitoring"
  password: "secret"
settings:
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

func TestParseConfig(t *testing.T) {
	config, err := parseYAML(t, validConfig)
	if err != nil {
		t.Fatal(err)
	}
	if config.Settings.Concurrency != 4 || config.Settings.TimeoutSeconds != 5 {
		t.Fatalf("unexpected defaults: %+v", config.Settings)
	}
	linux, windows := config.Hosts[0], config.Hosts[1]
	if linux.OS != "Linux" || linux.SSHPort != 22 || linux.SSHUser != "automation" {
		t.Fatalf("unexpected linux host: %+v", linux)
	}
	if windows.SSHPort != 2222 || windows.SSHUser != "" {
		t.Fatalf("unexpected windows host: %+v", windows)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string][2]string{
		"placeholder":     {`password: "secret"`, `password: "CHANGE_ME"`},
		"unknown key":     {`settings:`, "settings:\n  retries: 3"},
		"outside network": {`"192.0.2.10"`, `"198.51.100.10"`},
		"unknown os":      {`os: "linux"`, `os: "plan9"`},
		"option address":  {`"win.example"`, `"-oProxyCommand=x"`},
		"duplicate":       {`["nginx", "postgres"]`, `["nginx", "nginx"]`},
		"bad port":        {`ssh_port: 2222`, `ssh_port: 70000`},
		"no processes":    {`["spoolsv.exe"]`, `[]`},
	}
	for name, replacement := range cases {
		if _, err := parseYAML(t, strings.Replace(validConfig, replacement[0], replacement[1], 1)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestConfigSelection(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(work)

	// First run creates a private template and fails
	if _, _, err := loadConfig(""); err == nil || !strings.Contains(err.Error(), "template created") {
		t.Fatalf("unexpected error: %v", err)
	}
	info, err := os.Stat(userConfigPath(home))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected template: %v %v", info, err)
	}

	// Working directory wins over the user file
	local := filepath.Join(work, configFilename)
	if err := os.WriteFile(local, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, _, err := loadConfig(""); err != nil || path != local {
		t.Fatalf("unexpected selection: %s %v", path, err)
	}
	if _, _, err := loadConfig(filepath.Join(work, "missing.yml")); err == nil {
		t.Fatal("expected an error for a missing explicit path")
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

// Fake ssh client echoing fixed output
func fakeSSH(t *testing.T, script string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "ssh"), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
}

func TestCheckHost(t *testing.T) {
	host := RemoteHost{Name: "linux-01", Address: "192.0.2.10", OS: "Linux", Processes: []string{"nginx", "postgres"}, SSHPort: 22}
	limit := make(chan struct{}, 1)

	fakeSSH(t, `printf '1\n0\n'`)
	results := checkHost(context.Background(), host, limit, 1)
	if results[0].State != stateRunning || results[1].State != stateStopped {
		t.Fatalf("unexpected results: %+v", results)
	}

	fakeSSH(t, `echo 'Permission denied (publickey).' >&2; exit 255`)
	results = checkHost(context.Background(), host, limit, 1)
	if results[0].State != stateUnknown || results[1].Detail != "Permission denied (publickey)." {
		t.Fatalf("unexpected results: %+v", results)
	}

	fakeSSH(t, `exit 3`)
	if results = checkHost(context.Background(), host, limit, 1); results[0].Detail != "Remote process-check command is unavailable" {
		t.Fatalf("unexpected results: %+v", results)
	}

	fakeSSH(t, `echo 1`)
	if results = checkHost(context.Background(), host, limit, 1); results[0].Detail != "Remote process check returned invalid output" {
		t.Fatalf("unexpected results: %+v", results)
	}
}
