# Librescoot Update Service

Part of the [Librescoot](https://librescoot.org/) open-source platform.

The Update Service manages component-specific operating-system updates for MDB and DBC targets. Each instance discovers releases, downloads full or delta Mender artifacts, installs them with Mender, reports progress through Redis or Valkey, and coordinates reboot/power inhibition with the rest of the vehicle.
## Capabilities

- Runs independently for the required `mdb` or `dbc` component.
- Discovers releases from a configurable release-index base URL and supports `stable`, `testing`, and `nightly` channels.
- Downloads resumable Mender artifacts with configurable per-attempt duration and throughput budgets.
- Supports full and delta update methods; delta installation requires a compatible local base artifact.
- Accepts local-file and URL update requests, with optional SHA-256 verification.
- Recovers Mender state at startup, commits a booted pending update, and attempts rollback of inconsistent state.
- Optionally updates the U-Boot boot region from local boot assets when `--boot-update` is enabled.
- Publishes component status, progress, errors, heartbeats, and channel-preview results in the `ota` hash.
- Uses Redis/Valkey inhibitors and vehicle state to coordinate downloads, installation, and reboots.

## Operation and interfaces

Start the binary with exactly one component:

```sh
update-service --component=mdb
update-service --component=dbc
```

Each instance accepts commands on `scooter:update:<component>`:

| Command | Effect |
| --- | --- |
| `check-now` | Start an immediate release check. |
| `preview-channel:<channel>` | Resolve the latest matching artifact for a valid channel without starting an update. |
| `update-from-file:<path>` | Install a local `.mender` or `.delta` artifact. |
| `update-from-url:<url>` | Download and install an artifact from `http`, `https`, or `file` URL. |

For `update-from-file` and `update-from-url`, append `#sha256=<hex>` to request checksum verification. The legacy `:sha256:<hex>` suffix is also accepted. An unverified source is allowed when no checksum is supplied.

The service stores component-scoped data in the `ota` hash, including `status:<component>`, `update-version:<component>`, download and install progress, and error details. Primary statuses are `idle`, `downloading`, `preparing`, `installing`, `pending-reboot`, and `error`. Channel preview output is published as `preview-channel:<component>`, `preview-status:<component>`, `preview-version:<component>`, and `preview-size:<component>`.

Installed versions are read from `version:<component>` field `version_id`; the release variant is read from `variant_id`. Ensure the version service has published these fields before relying on automated channel selection.

## Configuration

| Flag | Default | Purpose |
| --- | --- | --- |
| `--component` | required | Target component: `mdb` or `dbc` |
| `--redis-addr` | `localhost:6379` | Redis/Valkey address |
| `--releases-url` | `https://downloads.librescoot.org/releases` | Release-index base URL |
| `--channel` | inferred | Update channel; must be `stable`, `testing`, or `nightly` |
| `--check-interval` | `6h` | Periodic check interval; `0` or `never` disables periodic checks |
| `--download-dir` | `/data/ota/{component}` | Artifact storage directory |
| `--dry-run` | `false` | Do not reboot after a successful update path |
| `--boot-update` | `false` | Enable boot-region update support |
| `--boot-mount` | `/uboot` | Boot partition mount point for device detection |
| `--boot-device` | auto-detected | U-Boot device path |
| `--boot-uboot-seek` | `2` | 512-byte blocks to skip before writing U-Boot |
| `--download-max-duration` | `60m` | Per-attempt download wall-clock limit; `0` disables it |
| `--download-stall-window` | `2m` | Throughput evaluation window; `0` disables it |
| `--download-stall-min-bytes` | `65536` | Bytes required in each stall window |
| `--version` | — | Print the build version and exit |

When not overridden by CLI values, the service loads and watches these component-scoped fields in the `settings` hash: `updates.<component>.channel`, `check-interval`, `releases-url`, `dry-run`, `download-max-duration`, `download-stall-window`, and `download-stall-min-bytes`. `never` disables the configured check interval. The update method is read from `updates.<component>.method`; supported values are `full` and `delta`.

## Build and test

A Go toolchain is required. The default target builds a static Linux ARMv7 executable.

```sh
make build       # bin/update-service for ARMv7
make build-host  # bin/update-service for the current host
make test
```

For local dry-run instances, use `make run-mdb` or `make run-dbc`. The Makefile also provides `make fmt`, `make deps`, `make lint`, and `make clean`.

## Deployment and runtime dependencies

The image recipe installs `/usr/bin/update-service`, `mender-apply-delta.py`, and one board-specific unit as `librescoot-update.service`. Both units run as `root`, restart automatically, create their required `/data/ota` directories before start, and set `GOMEMLIMIT=100MiB`.

The MDB unit requires Valkey and orders itself after network, modem, vehicle, version, power-management, and settings services. The DBC unit uses the MDB Redis/Valkey address `192.168.7.1:6379` and orders itself after network, version, and settings services.

Runtime dependencies include Redis or Valkey, the Mender command-line tooling and state storage, network access to the configured release index for remote updates, sufficient storage in the download directory, and the vehicle/power services used for inhibition and reboot coordination. Boot updates additionally require a valid boot mount/device and `/usr/share/boot-assets/u-boot-dtb.imx`.

```sh
systemctl status librescoot-update.service
journalctl -u librescoot-update.service
```

## Operational and security notes

- Update artifacts are privileged inputs. Use trusted release endpoints, protect local staging directories, and provide SHA-256 checksums for manually supplied artifacts.
- The service can invoke Mender and, with boot updates enabled, write and verify a U-Boot image in the boot region. Do not enable or run it with untrusted configuration or device paths.
- A pending Mender update is committed on the next successful startup after reboot. Inspect the `ota` hash and journal before clearing errors or replacing staged artifacts.
- `--dry-run` suppresses rebooting; it does not turn remote discovery, downloads, or all installation preparation into a no-op. Use it only with an appropriate test environment.

## License

This project is licensed under the [GNU Affero General Public License v3.0](LICENSE).

Made with ❤️ by the Librescoot community
