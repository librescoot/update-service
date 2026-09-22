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
- Optionally commits only after the new image proves healthy on the vehicle (`--commit-gate`): a window that fails or expires is rolled back through Mender and the bootloader.
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

Release discovery reads `latest.json`, the release index's one release per channel, which is also what lets the MDB judge the DBC against the DBC's own channel rather than its own. It is also the smaller document: one manifest for every channel replaces the newest channel's full release list on every check. A channel's `<channel>.json` list is fetched only where it adds something: a delta chain is built across several releases, and when the manifest carries no usable release for the channel and variant — a board that failed to build that night, a version the gate rolled back, or a manifest that has fallen behind — the list is consulted for an older release.

`update-from-file` behaves as before apart from the power holds: one `.mender` is a full image and one `.delta` is applied against the base image of the running version. A single delta is the one-file case of the chain path below, so it is prechecked the same way and takes the suspend hold the chain path takes for the duration of the apply. Both branches install through the same inhibit/tail as the staged path. For a full image the install inhibit is now taken before the checksum verification, so `installing` is published after the verification instead of before it; the delta success log line still names the base -> target it assembled.

`apply-staged-updates` is path-free. UMS stages artifacts in the canonical component download dir (`/data/ota/mdb`, or `/data/ota/dbc` on the DBC) and pushes this one command, and update-service owns the discovery. It resolves what to install as follows:

- `.mender` files that are not newer than the running version are the delta bases and old full images; as candidates they are ignored, so they never make a set ambiguous. The base for the running version has to be present for a delta to apply, so it is never a conflict. Retention is the startup sweep's keep-set described below, not this rule: that sweep keeps the running version's base plus a staged target and drops what is neither at the next startup.
- a `.mender` newer than the running version together with any delta that counts as a real candidate (one that parses as a newer artifact on the running version's channel), or two or more newer `.mender` files, is ambiguous: the whole set is refused (`staged-updates-refused`) and nothing is installed. A `.mender` on another channel is never a target and is ignored, however new its timestamp. A `.delta` the version test cannot judge — a cross-channel orphan, an unparsable name, or a partial transfer — is not a candidate: it is ignored rather than counted, so it neither joins a chain nor blocks a legitimately staged image, and it is left to the retention sweep below.
- one newer `.mender` on the running channel is installed as a full image. Otherwise the deltas newer than the running version are validated as one channel, strictly increasing chain that starts at the running version.
- when nothing in the dir is newer than the running version — the normal post-success state, since the staged image has become the running version — the command logs it and goes idle. It is not a refusal.

A delta chain is resolved by matching each delta's recorded old payload checksum to the previous delta's new one. Two deltas built for the same base (a fork), a delta that cannot be placed in the chain, or a multi-delta set whose chain cannot be checksum-resolved (a base whose manifest cannot be read, or a delta predating the payload-checksum fields) refuses the whole set with a clear error; a subset is never applied. The chain is then applied in a single unpack/apply/repack cycle and installed with one mender write and one reboot. A chain whose base does not match the staged image is refused (`delta-base-mismatch`) and a running version with no staged base is refused (`no-base-image`) before anything is unpacked.

The apply attempt cleans up after itself. A successful `ApplyDownloadedDelta`/`ApplyDownloadedDeltaChain` removes the `.delta` file(s) it consumed and the old base `.mender`; a non-cancelled failure also removes the `.delta` file(s); only a cancelled apply keeps them for the next attempt. On top of that, `CleanupStaleMenderFiles` and `CleanupStaleDeltaFiles` sweep the component download dir itself (`/data/ota/<component>` by default, the same dir UMS stages into), so that keep-set is the bound: the `.mender` keep-set is the running version's base plus the newest newer artifact on the same channel (a staged target), and `CleanupStaleDeltaFiles` reaps a `.delta` whose channel matches the newest local `.mender` token and that is not newer than it. A delta that can never apply is reaped at once rather than waiting for the backstop: a name carrying no recognizable target version (a manual or `file://` transfer such as `update.delta`), or — while some other channel does have a local base `.mender` — one whose own channel has no base to apply against. A `.delta.tmp` partial goes through the same switch as any delta, so it can be reaped by those can-never-apply rules and otherwise reaches the backstop through the version rule. What remains judgeable-but-unusable waits for the 30-day age backstop — a same-channel delta newer than a stale local base, or a cross-channel delta that does have a base on its own channel and so could become applicable after a channel switch. A `.delta.tmp` partial is judged by the same version rule (a partial at or below the newest local `.mender` token on its channel is reaped at once), and only reaches the backstop through that rule. A staged set that is never consumed can therefore linger for up to 30 days, and nothing in this retention path installs anything: it is removed without an install or simply sits until the backstop reaps it.

The service stores component-scoped data in the `ota` hash, including `status:<component>`, `update-version:<component>`, download and install progress, and the latest error. Every failure is also appended atomically to the `ota:errors` Redis Stream with `event=error`, `component`, `code`, and `message`; lifecycle resets append `event=reset` for that component. Consumers reconstruct the current operation by reading backwards to its latest reset, so a failed delta and a failed full-image fallback are both retained. `error:<component>` and `error-message:<component>` remain the backwards-compatible latest-error view, while `error-event:<component>` changes on every append to notify hash/pub-sub consumers that the stream advanced. `update-version:<component>` is the target of an active operation; it is not the running version. Primary statuses are `idle`, `downloading`, `preparing`, `installing`, `pending-reboot`, `staged-noop`, and `error`. `staged-noop` is terminal and not an error: an `apply-staged-updates` push resolved to nothing applicable, so nothing was installed and nothing needs rebooting. Channel preview output is published as `preview-channel:<component>`, `preview-status:<component>`, `preview-version:<component>`, and `preview-size:<component>`.

Running versions are read from `version:<component>` field `version_id`; the release variant is read from `variant_id`. During startup recovery, Mender's committed artifact and `standalone-state.ArtifactName` are the durable sources for committed and installed-but-uncommitted identity. The active rootfs `/etc/os-release` `VERSION_ID` must match the pending artifact before commit.

### Commit gate

The commit gate defers that commit until the platform has proved itself on the new image, so an update's success is "booted and the vehicle came up on it" rather than only "the running version matches the pending artifact".

It is on by default for nightly MDB updates, and off everywhere else, including the DBC on nightly: no gated DBC update has been through a real dashboard yet, so that stays opt-in until one has. An explicit choice always wins: `--commit-gate` on the command line, or `updates.mdb.commit-gate` for MDB commits and `updates.dbc.commit-gate` for DBC commits. Clearing the setting returns the device to its default, so switching a device to nightly enables the gate on the MDB and a channel change back disables it again. Enabling the gate changes the failure mode of a commit to fail-closed, so anywhere it is not on by default it is switched on per device once that device's probe set has been confirmed.

A window opens at startup when the gate is enabled and Mender's pending artifact is the version that is running. The component stays in `pending-reboot` throughout. Once the image has been up past the floor, these probes are evaluated every 15 seconds and every one must hold:

- uptime, from `/proc/uptime`, so the wall clock is not involved
- `systemctl is-system-running` is `running` or `degraded`
- every configured required unit is satisfied: active, or a oneshot that ran successfully during this boot
- vehicle-service has published a `vehicle` state
- `power-manager[state]` is `running`, not a power transition — MDB only
- the component still holds its image: `status:<component>` is `pending-reboot` with no error recorded

The floor is per component: three minutes on the MDB, one minute on the DBC, which reaches multi-user about 22 seconds after boot. On the DBC that keeps the window short, because the question the DBC has to answer is mostly whether it can reach the MDB again.

A reboot inside the window is not a verdict. A quick off-and-on, or any crash, comes back on the same uncommitted artifact and the window carries on, with the new boot recorded in the marker. Only a boot that has already asked for a rollback, or one that comes back on the committed slot, is treated as an outcome.

The deadline counts evaluating time, accumulated in fifteen-second ticks while the gate runs. A vehicle that spends hours powered off mid-window resumes with the budget it had left; an image that never earns its verdict still fails closed once the budget is spent.

While it waits, the gate also resets U-Boot's `bootcount`. U-Boot reverts an uncommitted slot once `bootcount` passes `bootlimit`, which a window that deliberately waits would otherwise trip on any power cycle or unrelated reboot, including the `performLocalBootUpdate` reboot. A boot that cannot run the gate does not reset it, so an image too broken to start userspace is still reverted by the bootloader.

The required units default to `valkey`, `librescoot-vehicle`, `librescoot-settings` and `librescoot-version`, plus `librescoot-pm` on the MDB. On the DBC they default to `librescoot-version` and `dbc-dispatcher`: vehicle, settings and pm-service run on the MDB and cannot be required from there, and although the DBC image ships valkey for its `redis-cli`, the DBC talks to the valkey instance on the MDB, so the recipe disables the DBC's valkey service and its unit never runs. What the DBC needs from the MDB is covered by the vehicle probe instead, which reads through the MDB's Redis, and its power state is deliberately not a probe: the MDB holds dashboard power for a DBC update and resumes suspending once the lifecycle completes, so that state may change while the window is still open without saying anything about the dashboard's image.

Modem, uplink, battery, ecu and keycard are deliberately absent: they legitimately fail or are absent depending on SIM, card and fitted hardware, and a required unit that is wrongly listed turns a good update into a rollback. Listing a oneshot is safe because the probe accepts a unit that ran successfully during this boot even when it is inactive afterwards, which is what `Type=oneshot` with `RemainAfterExit=no` always reports. A unit list that is wrong for a device is the main way this feature costs an update attempt, which is why each component's default is only as large as what its image guarantees.

All probes holding commits the update through the same path as an ungated startup. A hard failure, or the deadline expiring with a probe still failing, fails the window closed:

- the attempt is recorded in `/data/ota/commit-gate-<component>.json` with the probe that never passed
- Mender is asked to discard the pending state and the artifact is added to `/data/ota/commit-gate-quarantine-<component>.json`
- the component is set to `error` with code `commit-gate-rollback`
- a reboot is requested, which the bootloader answers by booting the previously committed slot

The record survives the reboot, and the next boot reads it: still running the pending artifact means the rollback did not land, so the gate clears Mender's stale state, keeps the artifact quarantined and reports `commit-gate-stuck` instead of rebooting again. Running the committed artifact instead means the bootloader reverted, which closes the window out to `idle`; without that, the MDB would hold `pending-reboot` for good, because the generic recovery path answers "reboot still required" and nothing on the MDB ever reboots.

On the DBC the window replaces the activation-attempt marker as the authority on this activation's outcome. The gate owns both records: a verified commit clears the activation attempt through the same path an ungated commit does and emits `complete-dbc`; a revert or a stuck rollback closes it as well, because that record is only read in the pending-commit branch and would otherwise outlive the decision without ever being examined again. A window therefore delays `complete-dbc` until its verdict, which is safe against vehicle-service's DBC update watchdog: that watchdog resets on any `ota:dbc` field change and the window keeps `heartbeat:dbc` ticking for its whole duration.

A quarantined artifact is not installed by the staged-file path and is not selected by a release check. It is dropped from the quarantine once the running version reaches or passes it, so a newer release is never affected.

Consumers read `commit-gate:<component>` for `waiting`, `committed`, `rolled-back`, `rollback-stuck`, `abandoned` or `disabled-runtime`, plus `commit-gate-reason:<component>` and `commit-gate-deadline:<component>`. Terminal statuses and startup initialization clear all three. A running window keeps the ordinary `heartbeat` field ticking, so consumers such as vehicle-service's update watchdog can tell a settling update from a wedged one.

While a window is open, `update-from-file`, `update-from-url` and `apply-staged-updates` are refused. The refusal is logged and deliberately does not write a status, because those handlers' own failure paths publish the same fields the gate reads as evidence about the image under test. Release checks already defer while the component is not idle.

The gate needs the bootloader to revert an uncommitted slot, which is what lands a rollback. Mender's U-Boot integration provides it with `bootlimit=1`, `bootcount` and `altbootcmd` in the boot environment, and both machines enable `mender-uboot`; confirm it on the specific board with `fw_printenv bootcount bootlimit upgrade_available`. Two reboots with `upgrade_available=1` and an untouched `bootcount` revert the slot: the first takes `bootcount` to 1, the second to 2, past `bootlimit`. If a board does not revert at all, the rollback has no way to land and the gate falls back to clearing Mender's state, quarantining the artifact and reporting, without rebooting.

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
| `--commit-gate` | per component | Commit a pending update only after the platform proves healthy on it (on for nightly MDB, off elsewhere) |
| `--commit-gate-floor` | component | Monotonic uptime before the commit gate evaluates its probes (`3m` on the MDB, `1m` on the DBC) |
| `--commit-gate-deadline` | `20m` | How long the commit gate may wait for its probes before rolling back |
| `--commit-gate-required-units` | component set | Comma-separated systemd units the commit gate requires active |
| `--version` | — | Print the build version and exit |

When not overridden by CLI values, the service loads and watches these component-scoped fields in the `settings` hash: `updates.<component>.channel`, `check-interval`, `releases-url`, `dry-run`, `download-max-duration`, `download-stall-window`, and `download-stall-min-bytes`. `never` disables the configured check interval. The update method is read from `updates.<component>.method`; supported values are `full` and `delta`. The commit gate reads `updates.<component>.commit-gate`, `commit-gate-floor`, `commit-gate-deadline`, and `commit-gate-required-units`, with the CLI flag winning as it does for the others; clearing `commit-gate` restores the component's default instead of forcing it off.

Enabling the gate on a running vehicle takes effect at the next startup: a window is opened by startup reconciliation, not by the setting changing. Disabling it takes effect immediately, and a window that is already open then commits without a verdict.

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
- With `--commit-gate` enabled, that commit also has to earn a health verdict, and a window that fails or expires rolls the update back and reboots. Treat the U-Boot boot counter check above as a prerequisite for enabling it on a board, and watch `commit-gate:<component>` and the `ota:errors` stream across the first real boots.
- `--dry-run` suppresses rebooting; it does not turn remote discovery, downloads, or all installation preparation into a no-op. Use it only with an appropriate test environment.

## Boot-write safeguards

Boot updates require `/usr/share/boot-assets/u-boot-dtb.imx` and its exact SHA-256 entry in the packaged `manifest.sha256`. Source reads are bounded; checksum and IVT/BootData validation run before comparing or writing the target. The opened target must match the kernel's boot0 block-device identity and capacity, and the declared image must fit. Identical images are not rewritten; comparison, write, sync, readback, and read-only restoration errors abort the operation.

Before writing, the service requires settled Mender state and a non-expiring `block` inhibitor acknowledged by pm-service in `power-manager:busy-services`, with `power-manager[state]` still `running`. DBC writes also wait for vehicle-service's `vehicle[dbc-updating]` acknowledgement and maintain a heartbeat. Failed prerequisites prevent the write. Startup clears orphaned boot inhibitors; operation cleanup releases its holds after the writer returns. A failed write does not request a reboot.

These checks detect corrupt inputs and reduce unsafe writes; they do not prove board compatibility, provide image authentication, or make an in-place write survive forced power loss. Device detection still selects boot0 and does not determine whether the ROM boots from it. This service does not migrate boot regions or provide bootloader A/B/fallback.

## License

This project is licensed under the [GNU Affero General Public License v3.0](LICENSE).

Made with ❤️ by the Librescoot community
