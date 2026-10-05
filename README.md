# 🔎 processprobe

One-shot Linux CLI that connects to Linux and Windows hosts over SSH, checks whether configured
processes are running, and writes each result to PostgreSQL. Ships as a single static binary.

## ✨ Features

- 🐧 **Linux hosts**: exact process-name match with `pgrep -x`.
- 🪟 **Windows hosts**: case-insensitive match with PowerShell `Get-Process`; `.exe` suffix optional.
- 🔐 **One SSH connection per host**: non-interactive batch mode with strict host keys.
- ⚡ **Concurrent**: configurable SSH concurrency and timeout.
- 🛡️ **Network allowlist**: optional CIDR restriction for literal host IPs.
- 💾 **Upsert**: updates rows matched by `(name, host, os)` and inserts missing ones in one transaction.
- ✅ **Strict configuration**: unknown keys, placeholders, and duplicates are rejected before any check.
- 📝 **First run**: creates a user-only config template.
- 🌈 **Output**: colored with emojis; plain when redirected, `NO_COLOR` is set, or `TERM=dumb`.

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

Without `--config`, the first existing file is used, without merging:

1. `config.yml` in the current working directory
2. `~/.config/processprobe/config.yml`

If neither exists, the second one is created with placeholders and the run exits `1`. See
[`config.example.yml`](config.example.yml):

| Setting | Description | Default |
| --- | --- | --- |
| `database.host`, `port`, `name`, `user`, `password` | PostgreSQL target | required |
| `settings.ssh_concurrency` | Parallel SSH connections | `4` |
| `settings.ssh_timeout_seconds` | SSH connect timeout; hard limit is 5 s longer | `5` |
| `settings.allowed_networks` | CIDR allowlist for literal host IPs; DNS names always pass | `[]` |
| `hosts[].name` | Identity stored in the `host` column | required |
| `hosts[].address` | DNS name or IP passed to SSH | required |
| `hosts[].os` | `Linux` or `Windows`, case-insensitive | required |
| `hosts[].processes` | Non-empty list of exact process names | required |
| `hosts[].ssh_user` | SSH account | SSH config |
| `hosts[].ssh_port` | SSH port | `22` |

Linux names are case-sensitive and truncated by the kernel to 15 characters; use `ps -eo comm`.

## 🚀 Usage

```bash
processprobe
processprobe --config /path/to/config.yml
processprobe --help
```

Authentication must be non-interactive and every host key must already be in `known_hosts`:

```bash
ssh automation@linux-server-01 true
ssh automation@windows-server-01 powershell.exe -NoProfile -Command Get-Process
```

| Exit | Meaning |
| --- | --- |
| `0` | Checks finished and all results were saved |
| `1` | Configuration or database error |
| `2` | Invalid command-line arguments |
| `130` | Interrupted by the user |

## ⏱️ Deploy

Install the binary, place the configuration, then schedule it with the example systemd units:

```bash
sudo install -m 644 processprobe.example.service /etc/systemd/system/processprobe.service
sudo install -m 644 processprobe.example.timer /etc/systemd/system/processprobe.timer
sudo systemctl daemon-reload
sudo systemctl enable --now processprobe.timer
journalctl -u processprobe.service
```

Adjust `User=` in the service; that account needs `~/.config/processprobe/config.yml` and its SSH keys.

## 🧪 Develop

```bash
go vet ./...
go test ./...
```

## 📜 License

MIT, see [LICENSE](LICENSE).
