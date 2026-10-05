package main

import (
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode"
)

// Operating systems as stored in the database
const (
	osLinux   = "Linux"
	osWindows = "Windows"

	defaultSSHPort = 22
)

// Must match config.yml.example
const defaultConfig = `database:
  host: "127.0.0.1"
  port: 5432
  name: "monitoring"
  user: "monitoring"
  password: "CHANGE_ME"

settings:
  ssh_concurrency: 4
  ssh_timeout_seconds: 5
  allowed_networks: []

hosts:
  - name: "linux-server-01"
    address: "192.0.2.10"
    os: "Linux"
    ssh_user: "automation"
    ssh_port: 22
    processes: ["nginx", "postgres"]
  - name: "windows-server-01"
    address: "198.51.100.10"
    os: "Windows"
    ssh_user: "automation"
    ssh_port: 22
    processes: ["spoolsv.exe", "notepad.exe"]
`

// PostgreSQL connection target
type DatabaseConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
}

// SSH limit, connect deadline, and optional CIDR allowlist
type Settings struct {
	Concurrency     int
	Timeout         time.Duration
	AllowedNetworks []netip.Prefix
}

// Host with the processes to look for
type RemoteHost struct {
	Name      string
	Address   string
	OS        string
	Processes []string
	SSHUser   string
	SSHPort   int
}

// Validated configuration
type Config struct {
	Database DatabaseConfig
	Settings Settings
	Hosts    []RemoteHost
}

// Strict validation of the decoded YAML document
func parseConfig(document any) (Config, error) {
	r := &reader{}
	root := r.mapping(document, "configuration")
	database := r.mapping(root["database"], "database")
	settings := r.mapping(root["settings"], "settings")
	r.exactKeys(root, "configuration", "database", "settings", "hosts")
	r.exactKeys(database, "database", "host", "port", "name", "user", "password")
	r.exactKeys(settings, "settings", "ssh_concurrency", "ssh_timeout_seconds", "allowed_networks")

	config := Config{
		Database: DatabaseConfig{
			Host:     r.text(database, "host", "database"),
			Port:     r.port(database, "port", "database"),
			Name:     r.text(database, "name", "database"),
			User:     r.text(database, "user", "database"),
			Password: r.text(database, "password", "database"),
		},
		Settings: Settings{
			Concurrency:     r.positiveInt(settings, "ssh_concurrency", "settings"),
			Timeout:         r.positiveSeconds(settings, "ssh_timeout_seconds", "settings"),
			AllowedNetworks: r.networks(settings),
		},
	}
	config.Hosts = r.hosts(root["hosts"], config.Settings.AllowedNetworks)
	if r.err != nil {
		return Config{}, r.err
	}
	return config, nil
}

// CIDR allowlist; bare addresses count as single-host networks
func (r *reader) networks(settings map[string]any) []netip.Prefix {
	items, ok := settings["allowed_networks"].([]any)
	if !ok {
		r.fail("settings.allowed_networks must be a list of CIDR networks")
	}
	var networks []netip.Prefix
	for index, item := range items {
		network, err := parseNetwork(fmt.Sprint(item))
		if err != nil {
			r.fail("settings.allowed_networks[%d] must be an IP address or CIDR network", index)
		}
		networks = append(networks, network)
	}
	return networks
}

func parseNetwork(value string) (netip.Prefix, error) {
	if !strings.Contains(value, "/") {
		address, err := netip.ParseAddr(value)
		return netip.PrefixFrom(address, address.BitLen()), err
	}
	network, err := netip.ParsePrefix(value)
	return network.Masked(), err
}

// Hosts with unique process identities inside the allowed networks
func (r *reader) hosts(value any, networks []netip.Prefix) []RemoteHost {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		r.fail("hosts must be a non-empty list")
	}
	var hosts []RemoteHost
	// Database rows are matched by (name, host, os)
	identities := map[[3]string]bool{}
	for index, item := range items {
		location := fmt.Sprintf("hosts[%d]", index)
		entry := r.mapping(item, location)
		keys := []string{"name", "address", "os", "processes"}
		for _, optional := range []string{"ssh_user", "ssh_port"} {
			if _, ok := entry[optional]; ok {
				keys = append(keys, optional)
			}
		}
		r.exactKeys(entry, location, keys...)

		host := RemoteHost{
			Name:    r.text(entry, "name", location),
			Address: r.sshComponent(entry, "address", location),
			OS:      r.system(entry, location),
			SSHPort: defaultSSHPort,
		}
		if _, ok := entry["ssh_user"]; ok {
			host.SSHUser = r.sshComponent(entry, "ssh_user", location)
		}
		if _, ok := entry["ssh_port"]; ok {
			host.SSHPort = r.port(entry, "ssh_port", location)
		}
		if address, err := netip.ParseAddr(host.Address); err == nil && len(networks) > 0 && !allowed(address, networks) {
			r.fail("%s.address is outside settings.allowed_networks", location)
		}

		processes, ok := entry["processes"].([]any)
		if !ok || len(processes) == 0 {
			r.fail("%s.processes must be a non-empty list", location)
		}
		for position, process := range processes {
			name, ok := process.(string)
			name = strings.TrimSpace(name)
			if !ok || name == "" {
				r.fail("%s.processes[%d] must be a non-empty string", location, position)
			}
			identity := [3]string{name, host.Name, host.OS}
			if identities[identity] {
				r.fail("%s.processes[%d] repeats a process of host %s", location, position, host.Name)
			}
			identities[identity] = true
			host.Processes = append(host.Processes, name)
		}
		hosts = append(hosts, host)
	}
	return hosts
}

// Value safe to pass as an SSH address or user
func (r *reader) sshComponent(values map[string]any, key, location string) string {
	value := r.text(values, key, location)
	if strings.HasPrefix(value, "-") || strings.ContainsFunc(value, func(char rune) bool {
		return unicode.IsSpace(char) || unicode.IsControl(char)
	}) {
		r.fail("%s.%s must not start with a dash or contain whitespace", location, key)
	}
	return value
}

// Linux or Windows, case-insensitive
func (r *reader) system(values map[string]any, location string) string {
	switch strings.ToLower(r.text(values, "os", location)) {
	case "linux":
		return osLinux
	case "windows":
		return osWindows
	}
	r.fail("%s.os must be %s or %s", location, osLinux, osWindows)
	return ""
}

// Literal addresses must lie in one of the networks
func allowed(address netip.Addr, networks []netip.Prefix) bool {
	for _, network := range networks {
		if network.Contains(address) {
			return true
		}
	}
	return false
}
