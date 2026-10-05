package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

const (
	appName        = "processprobe"
	configFilename = "config.yml"
)

const configTemplate = `database:
  host: "127.0.0.1"
  port: 5432
  name: "process_status"
  user: "status_user"
  password: "CHANGE_ME"

settings:
  ssh_concurrency: 4
  ssh_timeout_seconds: 5
  allowed_networks: []

hosts: []
`

// PostgreSQL connection target
type Database struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
}

// SSH limits and optional CIDR allowlist
type Settings struct {
	Concurrency     int
	TimeoutSeconds  int
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
	Database Database
	Settings Settings
	Hosts    []RemoteHost
}

// Exact path, else ./config.yml, else the user file; empty when none exists
func selectConfigPath(explicit string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if explicit != "" {
		path := explicit
		if strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
		if !isFile(path) {
			return "", fmt.Errorf("Configuration file not found: %s", explicit)
		}
		return path, nil
	}
	local, err := filepath.Abs(configFilename)
	if err != nil {
		return "", err
	}
	for _, candidate := range []string{local, userConfigPath(home)} {
		if isFile(candidate) {
			return candidate, nil
		}
	}
	return "", nil
}

func userConfigPath(home string) string {
	return filepath.Join(home, ".config", appName, configFilename)
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// First existing file wins; no merging, no fallback
func loadConfig(explicit string) (string, Config, error) {
	selected, err := selectConfigPath(explicit)
	if err != nil {
		return "", Config{}, err
	}
	if selected == "" {
		created, err := createConfigTemplate()
		if err != nil {
			return "", Config{}, err
		}
		return "", Config{}, fmt.Errorf("Configuration template created at %s; edit it and rerun", created)
	}

	data, err := os.ReadFile(selected)
	if err != nil {
		return "", Config{}, fmt.Errorf("Cannot read YAML configuration %s: %v", selected, err)
	}
	var document any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return "", Config{}, fmt.Errorf("Invalid YAML configuration %s%s", selected, yamlLocation(err))
	}
	config, err := parseConfig(document)
	return selected, config, err
}

var yamlLine = regexp.MustCompile(`line (\d+)`)

// Line number only; never echoes file content
func yamlLocation(err error) string {
	if match := yamlLine.FindStringSubmatch(err.Error()); match != nil {
		return " at line " + match[1]
	}
	return ""
}

// User-only placeholder file; an existing one is kept
func createConfigTemplate() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := userConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("Cannot create YAML configuration %s: %v", path, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("Cannot create YAML configuration %s: %v", path, err)
	}
	_, err = file.WriteString(configTemplate)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("Cannot create YAML configuration %s: %v", path, err)
	}
	return path, nil
}

// Validation failure raised while parsing
type configError struct{ error }

func fail(format string, args ...any) {
	panic(configError{fmt.Errorf(format, args...)})
}

// Strict validation of the decoded YAML document
func parseConfig(document any) (config Config, err error) {
	defer func() {
		switch failure := recover().(type) {
		case nil:
		case configError:
			config, err = Config{}, failure.error
		default:
			panic(failure)
		}
	}()

	root := mapping(document, "configuration")
	checkKeys(root, "configuration", []string{"database", "settings", "hosts"})
	database := mapping(root["database"], "database")
	settings := mapping(root["settings"], "settings")
	hosts, ok := root["hosts"].([]any)
	if !ok {
		fail("hosts must be a list")
	}
	checkKeys(database, "database", []string{"host", "port", "name", "user", "password"})
	checkKeys(settings, "settings", nil, "ssh_concurrency", "ssh_timeout_seconds", "allowed_networks")

	config.Database = Database{
		Host:     databaseText(database, "host"),
		Port:     positiveInt(database["port"], "database.port", 65535),
		Name:     databaseText(database, "name"),
		User:     databaseText(database, "user"),
		Password: databaseText(database, "password"),
	}
	config.Settings = Settings{
		Concurrency:     optionalInt(settings, "ssh_concurrency", "settings.ssh_concurrency", 4, 0),
		TimeoutSeconds:  optionalInt(settings, "ssh_timeout_seconds", "settings.ssh_timeout_seconds", 5, 0),
		AllowedNetworks: parseNetworks(settings),
	}
	config.Hosts = parseHosts(hosts)
	validateHostNetworks(config.Hosts, config.Settings.AllowedNetworks)
	return config, nil
}

func mapping(value any, path string) map[string]any {
	result, ok := value.(map[string]any)
	if !ok {
		fail("%s must be an object", path)
	}
	return result
}

// Reject missing required and unknown keys
func checkKeys(values map[string]any, location string, required []string, optional ...string) {
	known := map[string]bool{}
	var missing, unknown []string
	for _, key := range required {
		known[key] = true
		if _, ok := values[key]; !ok {
			missing = append(missing, key)
		}
	}
	for _, key := range optional {
		known[key] = true
	}
	for key := range values {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	var details []string
	if missing != nil {
		sort.Strings(missing)
		details = append(details, "missing: "+strings.Join(missing, ", "))
	}
	if unknown != nil {
		details = append(details, "unknown: "+strings.Join(unknown, ", "))
	}
	if details != nil {
		fail("%s keys are invalid (%s)", location, strings.Join(details, "; "))
	}
}

// Trimmed non-empty single-line string
func text(value any, path string) string {
	raw, ok := value.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		fail("%s must be a non-empty string", path)
	}
	if strings.ContainsAny(raw, "\x00\r\n") {
		fail("%s must not contain control characters", path)
	}
	return strings.TrimSpace(raw)
}

// Database string without leftover placeholder
func databaseText(database map[string]any, key string) string {
	value := text(database[key], "database."+key)
	if strings.EqualFold(value, "change_me") || strings.EqualFold(value, "change-me") {
		fail("database.%s still contains a placeholder", key)
	}
	return value
}

// Value safe to pass as an SSH address or user
func sshComponent(value any, path string) string {
	parsed := text(value, path)
	if strings.HasPrefix(parsed, "-") || strings.ContainsFunc(parsed, unicode.IsSpace) {
		fail("%s must be a valid SSH address or user component", path)
	}
	return parsed
}

// Positive integer; maximum 0 means unbounded
func positiveInt(value any, path string, maximum int) int {
	number, ok := value.(int)
	if !ok || number <= 0 || (maximum > 0 && number > maximum) {
		if maximum > 0 {
			fail("%s must be an integer between 1 and %d", path, maximum)
		}
		fail("%s must be a positive integer", path)
	}
	return number
}

func optionalInt(values map[string]any, key, path string, fallback, maximum int) int {
	value, ok := values[key]
	if !ok {
		return fallback
	}
	return positiveInt(value, path, maximum)
}

// CIDR allowlist; bare addresses count as single-host networks
func parseNetworks(settings map[string]any) []netip.Prefix {
	value, ok := settings["allowed_networks"]
	if !ok {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		fail("settings.allowed_networks must be a list of CIDR networks")
	}
	var networks []netip.Prefix
	for _, item := range items {
		network, err := parseNetwork(fmt.Sprint(item))
		if err != nil {
			fail("Invalid network in settings.allowed_networks: %v", err)
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

func parseHosts(items []any) []RemoteHost {
	if len(items) == 0 {
		fail("hosts must contain at least one host")
	}
	var hosts []RemoteHost
	identities := map[[3]string]bool{}
	for index, rawItem := range items {
		path := fmt.Sprintf("hosts[%d]", index)
		item := mapping(rawItem, path)
		checkKeys(item, path, []string{"name", "address", "os", "processes"}, "ssh_user", "ssh_port")

		rawProcesses, ok := item["processes"].([]any)
		if !ok || len(rawProcesses) == 0 {
			fail("%s.processes must be a non-empty list", path)
		}
		host := RemoteHost{
			Name:    text(item["name"], path+".name"),
			Address: sshComponent(item["address"], path+".address"),
			OS:      normalizeOS(item["os"], path+".os"),
			SSHPort: optionalInt(item, "ssh_port", path+".ssh_port", 22, 65535),
		}
		if item["ssh_user"] != nil {
			host.SSHUser = sshComponent(item["ssh_user"], path+".ssh_user")
		}
		for processIndex, process := range rawProcesses {
			name := text(process, fmt.Sprintf("%s.processes[%d]", path, processIndex))
			// Database rows are matched by (name, host, os)
			identity := [3]string{name, host.Name, host.OS}
			if identities[identity] {
				fail("Duplicate process identity configured: name=%q, host=%q, os=%q", name, host.Name, host.OS)
			}
			identities[identity] = true
			host.Processes = append(host.Processes, name)
		}
		hosts = append(hosts, host)
	}
	return hosts
}

func normalizeOS(value any, path string) string {
	switch strings.ToLower(text(value, path)) {
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	}
	fail("%s must be 'Linux' or 'Windows'", path)
	return ""
}

// Reject literal IPs outside the allowlist; DNS names pass
func validateHostNetworks(hosts []RemoteHost, networks []netip.Prefix) {
	if len(networks) == 0 {
		return
	}
	for _, host := range hosts {
		address, err := netip.ParseAddr(host.Address)
		if err != nil {
			continue
		}
		allowed := false
		for _, network := range networks {
			allowed = allowed || network.Contains(address)
		}
		if !allowed {
			fail("Host %q has address %q outside allowed networks: %s", host.Name, host.Address, joinNetworks(networks))
		}
	}
}

func joinNetworks(networks []netip.Prefix) string {
	names := make([]string, len(networks))
	for index, network := range networks {
		names[index] = network.String()
	}
	return strings.Join(names, ", ")
}
