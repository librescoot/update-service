# Librescoot Update Service

Part of the [Librescoot](https://librescoot.org/) open-source platform.

The Update Service manages component-specific operating-system updates for MDB and DBC targets. Each instance discovers releases, downloads full or delta Mender artifacts, installs them with Mender, reports progress through Redis or Valkey, and coordinates reboot/power inhibition with the rest of the vehicle.
## Capabilities

- Runs independently for the required `mdb` or `dbc` component.
- Discovers releases from a configurable release-index base URL and supports `stable`, `testing`, and `nightly` channels.
- Downloads resumable Mender artifacts with configurable per-attempt duration and throughput budgets.
- Supports full and delta update methods; delta installation requires a compatible local base artifact.
- Accepts local-file and URL update requests, with optional SHA-256 verification.
- Recovers pending state from Mender at startup, verifies the active rootfs, commits it, and verifies the resulting Mender state.
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
| `apply-staged-updates` | Discover the artifacts UMS staged in this component's download dir, resolve them, and install once. |
| `update-from-url:<url>` | Download and install an artifact from `http`, `https`, or `file` URL. |

For `update-from-file` and `update-from-url`, append `#sha256=<hex>` to request checksum verification. The legacy `:sha256:<hex>` suffix is also accepted. An unverified source is allowed when no checksum is supplied.

`update-from-file` keeps its existing behaviour byte for byte: one `.mender` is a full image and one `.delta` is applied against the base image of the running version. A single delta is the one-file case of the chain path below, so it is prechecked the same way.

`apply-staged-updates` is path-free. UMS stages artifacts in the canonical component download dir (`/data/ota/mdb`, or `/data/ota/dbc` on the DBC) and pushes this one command, and update-service owns the discovery. It resolves what to install as follows:

- `.mender` files that are not newer than the running version are the delta bases and old full images that live in this dir permanently; they are ignored. The base for the running version has to be present for a delta to apply, so it is never a conflict.
- a `.mender` newer than the running version together with any `.delta`, or two or more newer `.mender` files, is ambiguous: the whole set is refused (`staged-updates-refused`) and nothing is installed.
- one newer `.mender` is installed as a full image. Otherwise the deltas newer than the running version are validated as one channel, strictly increasing chain that starts at the running version.

A delta chain is resolved by matching each delta's recorded old payload checksum to the previous delta's new one. Two deltas built for the same base (a fork), or a delta that cannot be placed in the chain, refuses the whole set with a clear error; a subset is never applied. The chain is then applied in a single unpack/apply/repack cycle and installed with one mender write and one reboot. A chain whose base does not match the staged image is refused (`delta-base-mismatch`) and a running version with no staged base is refused (`no-base-image`) before anything is unpacked.

The service stores component-scoped data in the `ota` hash, including `status:<component>`, `update-version:<component>`, download and install progress, and error details. `update-version:<component>` is the target of an active operation; it is not the running version. Primary statuses are `idle`, `downloading`, `preparing`, `installing`, `pending-reboot`, and `error`. Channel preview output is published as `preview-channel:<component>`, `preview-status:<component>`, `preview-version:<component>`, and `preview-size:<component>`.

Running versions are read from `version:<component>` field `version_id`; the release variant is read from `variant_id`. During startup recovery, Mender's committed artifact and `standalone-state.ArtifactName` are the durable sources for committed and installed-but-uncommitted identity. The active rootfs `/etc/os-release` `VERSION_ID` must match the pending artifact before commit.

The MDB instance atomically maintains `/data/ota/dbc-state.json` after stable live DBC observations. If Redis data is lost while the DBC is powered off, it restores `version:dbc[version_id]` and the DBC OTA status/target with `ota[state-origin:dbc]=cached`. A running DBC instance sets that marker to `live`.

Each release check reads the current component channel and update method from settings before selecting a release; an explicit `--channel` still takes precedence. The selected channel stays fixed for that operation, including delta rechecks. Switching between recognized channels requests a full image, including nightly/testing to stable; same-channel stable checks still reject version downgrades. A settings read failure aborts the check instead of silently using a stale channel.

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
| `--boot-device` | auto-detected | Supported eMMC boot0 device (`/dev/mmcblkNboot0`); user-area targets are refused |
| `--boot-uboot-seek` | `2` | SD/eMMC IVT offset in 512-byte blocks; only `2` (1024 bytes) is supported |
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

The MDB unit requires Valkey and orders itself after network, modem, vehicle, version, power-management, and settings services. The DBC unit uses the MDB Redis/Valkey address `192.168.7.1:6379` and orders itself after network, version, and settings services. A successful DBC rootfs install keeps the `start-dbc` lifecycle active, waits for `stand-by`, `parked`, or `shutting-down`, requests a local DBC reboot, verifies and commits on startup, then emits `complete-dbc`; MDB dashboard-power cycling is not part of this activation path. UMS claims `ota[reboot-owner:mdb]=ums` around imported MDB updates so update-service leaves a combined install pending until UMS has observed the DBC's final outcome.

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

## Boot-write safeguards

Boot updates require `/usr/share/boot-assets/u-boot-dtb.imx` and its exact SHA-256 entry in the packaged `manifest.sha256`. Source reads are bounded; checksum and IVT/BootData validation run before comparing or writing the target. The opened target must match the kernel's boot0 block-device identity and capacity, and the declared image must fit. Identical images are not rewritten; comparison, write, sync, readback, and read-only restoration errors abort the operation.

Before writing, the service requires settled Mender state and a non-expiring `block` inhibitor acknowledged by pm-service in `power-manager:busy-services`, with `power-manager[state]` still `running`. DBC writes also wait for vehicle-service's `vehicle[dbc-updating]` acknowledgement and maintain a heartbeat. Failed prerequisites prevent the write. Startup clears orphaned boot inhibitors; operation cleanup releases its holds after the writer returns. A failed write does not request a reboot.

These checks detect corrupt inputs and reduce unsafe writes; they do not prove board compatibility, provide image authentication, or make an in-place write survive forced power loss. Device detection still selects boot0 and does not determine whether the ROM boots from it. This service does not migrate boot regions or provide bootloader A/B/fallback.

## License

This project is licensed under the [GNU Affero General Public License v3.0](LICENSE).

Made with ❤️ by the Librescoot community
