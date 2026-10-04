<div align="center">

# NexBoot: diskless boot server for Windows and Linux

**Boot a whole internet cafe, LAN center or computer lab from one master image. PXE + iSCSI + ZFS in a single Go binary, with a web console and an optional HA cluster.**

**English** · [简体中文](README.zh-CN.md)

[![Star](https://img.shields.io/github/stars/waterlemon0108/nexboot?style=for-the-badge&logo=github&color=yellow)](https://github.com/waterlemon0108/nexboot/stargazers)

[![License](https://img.shields.io/badge/license-AGPL--3.0-blue?style=for-the-badge)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://go.dev/)
[![Platform](https://img.shields.io/badge/platform-Linux%20x86__64-555?style=for-the-badge&logo=linux&logoColor=white)](#requirements)
[![CI](https://img.shields.io/github/actions/workflow/status/waterlemon0108/nexboot/ci.yml?branch=main&style=for-the-badge&label=CI)](https://github.com/waterlemon0108/nexboot/actions/workflows/ci.yml)

![NexBoot demo: from the web console to a client booting into Windows](docs/images/demo.gif)

</div>

## Why NexBoot

Keeping 50 or 500 PCs on the same Windows build is a chore. Every game patch, driver update or bad install means walking from seat to seat.

With NexBoot the PCs have no system disk of their own. They boot over the network, and each one gets its own writable copy of a master image. You update the image once. Every PC picks it up on the next reboot, and a reboot always brings a PC back to a clean state.

| Without NexBoot | With NexBoot |
|---|---|
| Patch every PC by hand | Patch one image, reboot the room |
| A broken PC needs a reinstall | A reboot restores it |
| Disks fail, PCs go down | No local system disk to fail |
| No easy way back after a bad update | Apply an earlier restore point in one click |

## Quick start

On an Ubuntu 22.04 / 24.04 server with a spare disk for the ZFS pool:

```sh
sudo apt install ./ndiskless_<version>_amd64.deb
sudo ndiskless-configure --bootstrap-password '<admin password>'
```

Then open `http://<server-ip>:8080`, sign in as `admin` and follow the setup wizard:

1. Pick the network card that faces the clients.
2. Create a storage pool on the spare disk.
3. Import a Windows or Ubuntu image (`.vmdk`, `.vhdx`, `.qcow2`, `.raw` or an exported `.zfs` stream).
4. Create a group (IP range + image) and register the clients by MAC address.
5. Set the clients to boot from the network. Done.

No internet at the site? Use the offline bundle: `sudo ./install.sh` from `nexboot-offline-ubuntu24.04-amd64.tar.gz`. See [Install](#install) for all options.

## How it works

```
 client PC                 NexBoot server (one Go binary)
 ─────────                 ──────────────────────────────────────────────
 PXE ROM ──DHCP/TFTP────▶  dnsmasq (managed)  ──▶ iPXE
 iPXE ────HTTP /boot────▶  boot script for this MAC (which image, which disk)
 iPXE ────iSCSI─────────▶  LIO target ──▶ ZFS clone of the restore point
 Windows / Linux boots     ▲
 from the network disk     └─ image ─▶ config ─▶ restore point ─▶ per-PC clone
```

- **Copy-on-write clones.** Each PC boots from a ZFS clone of a restore point. Clones cost almost no space and take a second to create.
- **Images, configs and restore points.** One image can have several configs (for example "esports" and "office"), and each config keeps its own history of restore points.
- **Super machine.** Boot one PC in edit mode, install your updates, shut it down and save the result as a new restore point.
- **Data plane stays local.** DHCP, `/boot` and iSCSI go to the node that hosts the PC. Nothing is proxied through a central controller.

## Features

| Area | What you get |
|---|---|
| Delivery | One static binary with the web UI built in; `.deb` package and an offline bundle for sites without internet |
| Client OS | Windows (BIOS and UEFI) and Ubuntu, including Ubuntu Server's default LVM layout |
| Image import | `.vmdk` / `.vhd(x)` / `.qcow2` / `.raw` / `.zfs` streams; Windows and Linux images are adapted for iSCSI boot automatically |
| Image management | Configs, restore points, one-click apply, save as new image, export, overwrite base image, health check |
| Groups and clients | Fixed IP per MAC via DHCP, data disks per group, client status matrix, cross-subnet groups via DHCP relay |
| Drivers | Driver pack import with INF parsing, applied to images through the super machine |
| Storage | ZFS pools with mirrors, cache and spare disks, managed from the UI; scheduled backups to a second pool |
| High availability | keepalived VIP, catalogue replication with resume, epoch fencing, planned switchover; grow from one node to a cluster online |
| Operations | Alarms (nodes, pools, replication, boot failures, image health), audit log, task center |

## Screenshots

| | |
|:---:|:---:|
| [![Overview](docs/images/demo/overview.jpg)](docs/images/demo/overview.jpg)<br/>Overview: nodes, clients, groups and storage at a glance | [![Images](docs/images/demo/images.jpg)](docs/images/demo/images.jpg)<br/>Images: system disks and data disks |
| [![Restore points](docs/images/demo/restore-points.jpg)](docs/images/demo/restore-points.jpg)<br/>Restore points: apply, save as image, export | [![Groups](docs/images/demo/groups.jpg)](docs/images/demo/groups.jpg)<br/>Groups: IP range, system disk and data disks |
| [![Clients](docs/images/demo/terminals.jpg)](docs/images/demo/terminals.jpg)<br/>Client matrix | [![Storage pools](docs/images/demo/storage.jpg)](docs/images/demo/storage.jpg)<br/>Storage pools per node |
| [![Create cluster](docs/images/demo/create-cluster.jpg)](docs/images/demo/create-cluster.jpg)<br/>Create a cluster with just a VIP | [![Add node](docs/images/demo/add-node.jpg)](docs/images/demo/add-node.jpg)<br/>Add nodes found on the network |
| [![Windows client](docs/images/demo/client-windows-desktop.jpg)](docs/images/demo/client-windows-desktop.jpg)<br/>Windows 11 client booted over the network | [![Ubuntu client](docs/images/demo/client-ubuntu-login.jpg)](docs/images/demo/client-ubuntu-login.jpg)<br/>Ubuntu 24.04 client booted over the network |

More screenshots are in the [Chinese README](README.zh-CN.md#演示图).

## Requirements

**Each server**

| Item | Requirement |
|---|---|
| OS | Ubuntu 22.04 / 24.04 (other systemd + ZFS distributions should work) |
| Disks | At least one disk besides the system disk, for the ZFS pool |
| Memory | 8 GB minimum, 32 GB or more recommended (enough ZFS ARC for your hot images) |
| Network | Same layer-2 segment as the clients; cluster nodes share it too (VRRP runs there) |
| IP address | Every server needs a **static IP**, not DHCP |
| Packages | `dnsmasq`, `dnsmasq-utils`, `qemu-utils`, `targetcli-fb`, `zfsutils-linux`, `keepalived`, `curl` (pulled in by the `.deb`, bundled in the offline package) |

**Network**

- No other DHCP server may answer the clients. NexBoot only answers registered MACs, and a second server would hand out wrong addresses.
- Set the clients to boot from the network card. BIOS and UEFI both work.

### Client images

| OS | Status | Before you capture the image |
|---|---|---|
| Windows | Supported | Set the network card to obtain an IP automatically. Windows 11 24H2 / 25H2 must be build 26100.7705 / 26200.7705 or later ([KB5074105](https://support.microsoft.com/en-us/topic/january-29-2026-kb5074105-os-builds-26200-7705-and-26100-7705-preview-85bd25de-894a-43eb-a19b-9a59d10f194b) fixes an iSCSI boot failure) |
| Ubuntu | Supported | `sudo apt install open-iscsi`. Plain partitions and linear LVM volumes both work |
| CentOS / RHEL / Rocky | Not adapted, untested | See the [Chinese README](README.zh-CN.md#客户机镜像要求) for the manual dracut setup |

Don't set a static IP inside the image. Every PC boots the same image, so a fixed address would be claimed by all of them at once. NexBoot gives each MAC its own address over DHCP. Ubuntu images are switched to DHCP automatically on import.

## Install

Every server runs the same install and starts as a standalone node. Whether it joins a cluster is decided later in the web UI.

```sh
# .deb package (recommended)
sudo apt install ./ndiskless_<version>_amd64.deb
sudo ndiskless-configure --bootstrap-password '<admin password>'

# offline bundle, for sites without internet
mkdir nexboot && tar -xzf nexboot-offline-ubuntu24.04-amd64.tar.gz -C nexboot && cd nexboot
sudo ./install.sh
sudo ndiskless-configure --bootstrap-password '<admin password>'

# from source (see Development for build prerequisites)
make build-static
sudo bash scripts/install-go.sh --install-deps --binary ./bin/ndiskless-linux-amd64 \
  --bootstrap-password '<admin password>'
```

Check it is up:

```sh
systemctl is-active ndiskless            # active
curl -s http://127.0.0.1:8080/healthz    # {"status":"ok"}
```

Upgrading a `.deb` does not restart the service. Run `sudo systemctl restart ndiskless`; in a cluster, upgrade the standbys first and the active node last.

## High availability

| | Single node | Two nodes | Three or more |
|---|---|---|---|
| Who accepts changes | This node | Active node | Active node |
| Who serves client disks | This node | Both, balanced (or pinned per group) | All nodes, balanced (or pinned per group) |
| One node fails | All clients down | Only its clients reboot once | Only its clients reboot once |

Failover is automatic and takes seconds to minutes, not zero. Clients running on a failed node blue-screen and reboot once, then come back on another node. The VIP moves in about a second.

To build a cluster:

1. On the first node: System → Parameters → High availability → Create cluster, and enter a free VIP.
2. Servers → Cluster nodes → Add node. NexBoot scans the client network for installed but unconfigured servers.
3. Create a storage pool on every node.
4. Wait for the first replication to finish (shown under High availability).

A single node that already has images and clients can be turned into a cluster in place. Nothing is reinstalled or migrated.

## Project structure

```
cmd/ndiskless/   entry point, wiring, HA role selection
internal/
  api/           HTTP routes, auth, /boot
  boot/          iPXE scripts and embedded TFTP assets
  control/       images, configs, restore points, groups, clients, drivers, alarms, backup, cluster ops
  dhcp/          dnsmasq config generation
  ha/            role state machine, epoch fencing, replication endpoints
  storage/       ZFS, iSCSI (LIO), image import and adaptation
  store/         SQLite repositories and goose migrations
web/             React web console, embedded into the binary
deploy/          systemd unit, keepalived template, health scripts
packaging/       .deb and offline bundle builders
tools/e2e/       end-to-end test matrices run against real servers
docs/images/     screenshots, demo GIF and project artwork
```

## Development

Use the Go version specified in `go.mod` and Node.js 22 (at least 22.22.2) or 24 (at least 24.15.0), with npm. Build and verification targets install frontend dependencies with `npm ci`, build `web/dist` and embed it before compiling Go. Generated files are not committed.

```sh
make dev          # dev binary at bin/ndiskless-dev
make verify       # gofmt, vet, go test, static build
make verify-web   # vitest, vite build
make verify-all   # all checks; shares the frontend build
make deb          # .deb package (on Linux)
make offline      # offline bundle (on the target Ubuntu version)
```

End-to-end matrices need real ZFS, LIO and keepalived, so they are not part of CI. See [`tools/e2e/README.md`](tools/e2e/README.md).

## Contributing

Issues and pull requests are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) first.

## License

[AGPL-3.0](./LICENSE). You can use, study and modify NexBoot for free, including running it in your own internet cafe or school. If you distribute a modified version, or offer a modified version to others over a network, you must publish its full source under the AGPL.

The NexBoot name and logo are not covered by the license. Bundled third-party components (iPXE, wimboot) keep their own licenses; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

---

<div align="center">

Built by [waterlemon0108](https://github.com/waterlemon0108). If NexBoot saves you a walk around the room, a star helps others find it.

[![Star](https://img.shields.io/github/stars/waterlemon0108/nexboot?style=for-the-badge&logo=github&color=yellow)](https://github.com/waterlemon0108/nexboot/stargazers)

</div>
