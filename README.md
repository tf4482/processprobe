# 🔎 processprobe

One-shot Linux CLI that connects to Linux and Windows hosts over SSH, checks whether configured
processes are running, and writes each result to PostgreSQL. Ships as a single static binary.

## ✨ Features

- 🐧 **Linux hosts**: exact process-name match with `pgrep -x`.
- 🪟 **Windows hosts**: case-insensitive match with PowerShell `Get-Process`; `.exe` suffix optional.
- 🔐 **One SSH connection per host**: non-interactive batch mode with strict host keys.
- ⚡ **Concurrent**: configurable SSH concurrency and a hard deadline per host.
- 🛡️ **Network allowlist**: optional CIDR restriction for literal host IPs.
- 💾 **Upsert**: updates rows matched by `(name, host, os)` and inserts missing ones in one transaction.
- ✅ **Strict configuration**: missing and unknown keys, placeholders, and duplicates are rejected before
  any check.
- 🧭 **Stable reasons**: unreachable hosts report a short cause such as `timeout` or
  `authentication failed`.
- 📝 **First run**: creates a user-only config template.
- 🌈 **Output**: colored with emojis; plain when redirected or `NO_COLOR` is set.

## 📦 Install

Building needs Go 1.26+. Running needs the OpenSSH client and PostgreSQL access; remote hosts need
`pgrep` (Linux) or PowerShell with an OpenSSH server (Windows).

```bash
git clone https://github.com/tf4482/processprobe.git
cd processprobe
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/processprobe .
sudo install -m 755 dist/processprobe /usr/local/bin/processprobe
```

## 🗄️ Database

```sql
CREATE TABLE IF NOT EXISTS processes (
    name TEXT NOT NULL,
    status BOOLEAN,
    host TEXT,
    os TEXT,
    last_check TIMESTAMP,
    UNIQUE (name, host, os)
);
```

`status` is `TRUE` only for a confirmed running process; stopped and unknown results are `FALSE`.
`last_check` is naive UTC. Duplicate `(name, host, os)` rows fail the run and roll back.

## ⚙️ Configuration

The first existing file is used, without merging:

1. `config.yml` beside the executable
2. `~/.config/processprobe/config.yml`

If neither exists, the second one is created with dummy values and the run exits `1`. Copy
[`config.yml.example`](config.yml.example) to one of these locations, replace `CHANGE_ME`, and set your
hosts:

| Setting | Description |
| --- | --- |
| `database.host`, `port`, `name`, `user`, `password` | PostgreSQL target |
| `settings.ssh_concurrency` | Parallel SSH connections |
| `settings.ssh_timeout_seconds` | SSH connect deadline; the whole check may take 5 s longer |
| `settings.allowed_networks` | CIDR allowlist for literal host IPs; DNS names always pass; `[]` disables it |
| `hosts[].name` | Identity stored in the `host` column |
| `hosts[].address` | DNS name or IP passed to SSH |
| `hosts[].os` | `Linux` or `Windows`, case-insensitive |
| `hosts[].processes` | Non-empty list of exact process names |
| `hosts[].ssh_user` | Optional SSH account; the SSH configuration decides without it |
| `hosts[].ssh_port` | Optional SSH port, `22` without it |

Linux names are case-sensitive and truncated by the kernel to 15 characters; use `ps -eo comm`.

## 🚀 Usage

```bash
processprobe
processprobe --help   # also: -h, help
```

Authentication must be non-interactive and every host key must already be in `known_hosts`:

```bash
ssh automation@linux-server-01 true
ssh automation@windows-server-01 powershell.exe -NoProfile -Command Get-Process
```

| Exit | Meaning |
| --- | --- |
| `0` | Every process was checked and saved; stopped processes are not errors |
| `1` | Configuration, dependency, database, or unreachable-host error |
| `130` | Interrupted by the user |

Processes on a host that could not be checked are reported as `unknown`, stored as `FALSE`, and make
the run exit `1`.

## ⏱️ Deploy

Install the binary, place the configuration, then schedule it with the example systemd units. The timer
waits for the previous run to finish, so runs never overlap.

```bash
sudo install -m 644 processprobe.example.service /etc/systemd/system/processprobe.service
sudo install -m 644 processprobe.example.timer /etc/systemd/system/processprobe.timer
sudo systemctl daemon-reload
sudo systemctl enable --now processprobe.timer
journalctl -u processprobe.service
```

Adjust `User=` in the service; that account needs the configuration and its SSH keys.

## 🧪 Develop

```bash
go vet ./...
go test ./...
```

## 📜 License

MIT, see [LICENSE](LICENSE).
