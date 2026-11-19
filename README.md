# chronytop

[![CI](https://github.com/geerlingguy/chronytop/actions/workflows/ci.yml/badge.svg)](https://github.com/geerlingguy/chronytop/actions/workflows/ci.yml)

A TUI for [chrony](https://chrony-project.org) NTP time server monitoring, in the style of [btop](https://github.com/aristocratos/btop).

![chronytop demo](resources/chronytop.gif)

It graphs NTP requests per second, and shows server counters, the clients chrony is rate limiting the most, sources, tracking, bandwidth, and CPU and memory use (including chronyd's).

Panels resize with the terminal, and can be shown or hidden.

## Requirements

- Linux, with chronyd 4.3 or later (older versions may work, but aren't tested)
- Root (or `sudo`) for the server and offenders panels, or see [Running without root](#running-without-root)

chronytop is a single static binary with no dependencies outside the Go standard library, so it runs on any distribution (Debian, Ubuntu, Fedora, RHEL, Arch, Alpine, etc.). It talks to chronyd directly over its Unix command socket, or over UDP with `-host`; `chronyc` isn't needed.

## Install

Download the binary for your architecture from [Releases](https://github.com/geerlingguy/chronytop/releases), and install it:

```
sudo install --owner=root --group=root --mode=755 chronytop-linux-amd64 /usr/local/bin/chronytop
```

Or build it from source with Go 1.24 or later:

```
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o chronytop .
```

Packagers can change the default socket path with `-ldflags "-X main.defaultSocket=/run/chrony/chronyd.sock"`.

## Usage

```
sudo chronytop
```

| Option | Default | Description |
| --- | --- | --- |
| `-i` | `1s` | Sample interval (500ms to 10s) |
| `-p` | `100` | Minimum packets for a client to be listed in offenders |
| `-socket` | `/var/run/chrony/chronyd.sock` | chronyd's Unix command socket |
| `-host` | | Monitor chronyd over UDP instead: host, `host:port` or `[IPv6]:port` (default port 323) |
| `-version` | | Print the version and exit |

| Key | Action |
| --- | --- |
| `1`-`7` | Show/hide a panel |
| `+` / `-` | Slower / faster sampling |
| `?` / `h` | Show/hide help |
| `Esc` | Close help, or quit |
| `q` | Quit |

Visible panels and the sample interval are saved when you quit, to `~/.config/chronytop/chronytop.conf` (`/root/.config/...` when run with `sudo`), and restored on the next run. `-i` overrides the saved interval.

The offenders list is refreshed every 10 seconds in the background. chronyd filters clients by `-p` itself, so the list stays cheap to fetch on busy public servers with hundreds of thousands of clients.

### Remote monitoring

`chronytop -host ntp.example.com` monitors another server, which needs to allow it in `chrony.conf`:

```
bindcmdaddress 0.0.0.0
cmdallow 192.0.2.20
opencommands activity manual rtcdata smoothing sourcename sources sourcestats tracking serverstats clients
```

`opencommands` (chrony 4.7+) replaces the defaults, so the list repeats them before adding `serverstats` and `clients`. The net and system panels aren't shown for remote hosts.

## Running without root

Without access to chronyd's default Unix socket, chronytop uses UDP on localhost, like `chronyc`. That shows everything except the server and offenders panels, since chronyd only answers `serverstats` and `clients` over UDP if allowed:

**chrony 4.7 or later:** add the `opencommands` line above to `chrony.conf` (`/etc/chrony/chrony.conf` on Debian and Ubuntu, `/etc/chrony.conf` elsewhere), restart chronyd, and run `chronytop` as any user. Only localhost can send these queries unless `cmdallow` says otherwise.

**Older versions:** adding your user to chrony's group (`_chrony` on Debian and Ubuntu, `chrony` on Fedora, RHEL, and Arch) only works if that group can write to `/run/chrony`, since chronytop creates its reply socket there. Check with `ls -ld /run/chrony`. If it shows `drwxrwx---`, add your user to the group, then log out and back in:

```
sudo usermod --append --groups _chrony $USER
```

Debian and Ubuntu don't make that directory group-writable, so this doesn't work there; use `sudo` instead. Where it does work, it gives full access to chronyd's control socket, so members of the group can also step the clock or change sources.

## Security

Since it usually runs as root, chronytop:

- Only sends read-only monitoring commands to chronyd, and never resets its counters or runs other programs.
- Only reads local stats from `/proc`, and only uses UDP with `-host` or the localhost fallback.
- Creates its reply socket in a private, randomly named directory beside chronyd's socket, and removes it on exit.
- Only writes its settings file, and only if you changed a setting. It skips the file and its directories unless they're owned by the user running chronytop (so a non-root `HOME` under `sudo` is ignored), and saves through a temporary file, so it never follows a symlink.
- Strips non-printable characters from everything it displays, so data from chrony can't inject terminal escape sequences.

CI runs `go vet`, `staticcheck`, `gosec`, and `govulncheck` on every change, and tests against chronyd 4.3, 4.6, and the latest release.

## Testing

```
go test -race ./...
CHRONYTOP_TEST_CHRONYD="$(command -v chronyd)" go test -run TestChronydIntegration -v
```

The first uses fake daemons; the second starts its own chronyd on loopback.

## License

MIT
