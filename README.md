# proxport

`proxport` is a small Go daemon for a Proxmox host with the public IP address. It listens on selected TCP or UDP ports on the host and forwards traffic to services running on VMs behind that host.

## What it does

- Listens on one or more TCP or UDP ports on the Proxmox host
- Forwards each incoming connection to a configured VM IP and port
- Uses a simple YAML config file
- Logs startup, connection failures, and disconnects
- Shuts down cleanly on `SIGTERM` and `Ctrl+C`

This is a user-space TCP/UDP proxy, not an iptables/nftables NAT helper. That makes it easier to understand, deploy, and run as a normal service.

## Project layout

```text
cmd/proxport/main.go
config.example.yaml
README.md
```

## Build

```bash
go build -o proxport ./cmd/proxport
```

For a Linux Proxmox host from another machine:

```bash
GOOS=linux GOARCH=amd64 go build -o proxport ./cmd/proxport
```

## Releases

GitHub Actions builds release artifacts automatically when you push a tag that starts with `v`, for example:

```bash
git tag v0.1.0
git push origin v0.1.0
```

Each release includes packaged binaries for Linux `amd64`, Linux `arm64`, and Windows `amd64`, plus a `checksums.txt` file.

## Config

Copy `config.example.yaml` to `config.yaml` and adjust the VM addresses and ports.

Example:

```yaml
listen_address: 0.0.0.0
dial_timeout: 5s
forwards:
  - name: ssh-vm-101
    protocol: tcp
    listen_port: 2222
    target_host: 192.168.100.101
    target_port: 22
  - name: game-vm-103
    protocol: udp
    listen_port: 27015
    target_host: 192.168.100.103
    target_port: 27015
```

Fields:

- `listen_address`: IP to bind on the Proxmox host, usually `0.0.0.0`
- `dial_timeout`: Timeout while connecting to the VM service
- `forwards[].name`: Friendly log label
- `forwards[].protocol`: `tcp` or `udp`, defaults to `tcp`
- `forwards[].listen_port`: Public port opened on the Proxmox host
- `forwards[].target_host`: VM IP address reachable from the host
- `forwards[].target_port`: Port on the VM service

## Run

```bash
./proxport ./config.yaml
```

If no argument is given, the app looks for `config.yaml` in the current directory. JSON is still accepted when you pass a `.json` file explicitly.

## systemd service example

```ini
[Unit]
Description=TCP port forwarder for Proxmox VMs
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/proxport
ExecStart=/opt/proxport/proxport /opt/proxport/config.yaml
Restart=always
RestartSec=3
User=root

[Install]
WantedBy=multi-user.target
```

`root` is commonly needed when binding ports below `1024` such as `80` or `443`.

## Notes for Proxmox

- Make sure the Proxmox firewall allows the listening ports.
- Make sure the host can route to the VM IP addresses.
- This proxy supports both TCP and UDP forwarding.
- UDP forwarding keeps temporary per-client sessions so reply packets from the VM are routed back to the correct internet client.
