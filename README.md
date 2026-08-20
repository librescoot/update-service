# Librescoot Update Service

A service for managing over-the-air (OTA) updates for Librescoot vehicles.

Part of the [Librescoot](https://librescoot.org/) open-source platform.

## Overview

The Update Service is responsible for:

- Checking a configurable endpoint (GitHub Releases API) for available updates specific to its component and channel (stable, testing, nightly).
- Orchestrating the download and installation of updates using Mender.
- Tracking download and installation progress via Redis.
- Managing power states and update inhibitions to ensure safe update application.
- Rebooting the specific component's system if necessary, adhering to defined constraints.

## Features

- **Component-Specific Instances**: Runs as separate, focused services for MDB and DBC updates.
- **GitHub Releases API Integration**: For update discovery.
- **Startup Commit Check**: Ensures that any update pending from a previous run is properly committed.
- **Power Management Integration**: Uses an inhibitor client to coordinate with vehicle power states, preventing updates during critical operations.
- **Safe Update Application**: Manages vehicle state and update inhibitions.
- **Controlled Reboots**: Schedules reboots based on component-specific rules and vehicle state (e.g., MDB reboots only in stand-by).
- **Dry-Run Mode**: Allows testing update logic without performing actual reboots.
- **Redis-Based State and Communication**: Uses Redis for status tracking and inter-service communication.
- **Flexible Update Sources**: Supports updates from local files or remote URLs (both full and delta updates).

## Installation

The service is typically built and installed using the provided `Makefile`.

```bash
# Clone the repository
git clone https://github.com/librescoot/update-service.git
cd update-service

# Build the distribution binary (ARM)
make dist
# This creates ./update-service-arm-dist

# Install the binary (requires root)
make install
# This copies ./update-service-arm-dist to /usr/bin/update-service

# Install systemd services (requires sudo)
# The repository includes service files like librescoot-update-mdb.service and librescoot-update-dbc.service.
# These should be copied to /etc/systemd/system/. For example:
sudo cp librescoot-update-mdb.service /etc/systemd/system/
sudo cp librescoot-update-dbc.service /etc/systemd/system/

# Then, enable and start the services:
sudo systemctl daemon-reload
sudo systemctl enable librescoot-update-mdb.service
sudo systemctl start librescoot-update-mdb.service
sudo systemctl enable librescoot-update-dbc.service
sudo systemctl start librescoot-update-dbc.service
```

## Usage

The service is typically run as a systemd service. Each instance (MDB, DBC) is configured via its respective service file.
The binary itself requires the `--component` flag.

Manual execution (example):
```bash
# Run for MDB component
./update-service --component=mdb --channel=nightly

# Run for DBC component with dry-run
./update-service --component=dbc --channel=stable --dry-run --redis-addr=127.0.0.1:6379
```

The `Makefile` provides convenience targets for running locally:
```bash
# Run for MDB (nightly, dry-run)
make run-mdb

# Run for DBC (nightly, dry-run)
make run-dbc
```

## Configuration

The service can be configured via command-line flags or Redis settings. CLI flags take precedence over Redis settings.

### Command-Line Flags

| Flag                    | Description                                                       | Default                                                        | Required | Redis Configurable |
|-------------------------|-------------------------------------------------------------------|----------------------------------------------------------------|----------|--------------------|
| `--component`           | Component to manage updates for.                                  | `""`                                                           | **Yes** (must be `mdb` or `dbc`) | No (CLI only) |
| `--redis-addr`          | Redis server address.                                             | `localhost:6379`                                               | No       | No (CLI only) |
| `--channel`             | Update channel to track.                                          | `nightly`                                                      | No       | Yes |
| `--releases-url` | Release index base URL for update discovery.                      | `https://downloads.librescoot.org/releases`                    | No       | Yes |
| `--check-interval`      | Interval between update checks.                                   | `6h`                                                           | No       | Yes |
| `--dry-run`             | If true, log reboot actions instead of performing them.           | `false`                                                        | No       | Yes |

**Note:** `--component` and `--redis-addr` are CLI-only and cannot be configured via Redis.

### Redis Settings

Settings can be configured per-component in the Redis `settings` hash. The update service monitors the `settings` channel for changes and applies them at runtime.

**Setting Keys:**
- `updates.{component}.channel` - Update channel (`stable`, `testing`, or `nightly`)
- `updates.{component}.check-interval` - Check interval (e.g., `6h`, `1h`, `30m`)
- `updates.{component}.releases-url` - Release index base URL
- `updates.{component}.dry-run` - Dry-run mode (`true` or `false`)
- `updates.{component}.method` - Update method (`full` or `delta`)

**Examples:**
```bash
# Set MDB to stable channel
redis-cli HSET settings updates.mdb.channel stable
redis-cli PUBLISH settings updates.mdb.channel

# Set DBC check interval to 12 hours
redis-cli HSET settings updates.dbc.check-interval 12h
redis-cli PUBLISH settings updates.dbc.check-interval

# Enable delta updates for MDB
redis-cli HSET settings updates.mdb.method delta
redis-cli PUBLISH settings updates.mdb.method

# Enable dry-run for testing
redis-cli HSET settings updates.dbc.dry-run true
redis-cli PUBLISH settings updates.dbc.dry-run
```

**Priority:** CLI flags (if specified) > Redis settings > hardcoded defaults

Many previous Redis key configurations are now handled internally based on the specified `--component`.

### Redis Commands

The update service listens for commands on the `scooter:update` list. Commands can be sent using Redis LPUSH.

**Available Commands:**

#### Standard Commands
- `check-now` - Immediately trigger an update check, bypassing the configured check interval
- `preview-channel:<channel>` - Report what a switch to `<channel>` would fetch, without changing any setting or starting a download. The answer lands in the `preview-*` fields of the `ota` hash (see [Redis Schema](#redis-schema))

#### Custom Update Sources
- `update-from-file:/path/to/file.mender` - Update from a local Mender file
- `update-from-file:/path/to/file.mender#sha256=checksum` - Update from local file with checksum verification
- `update-from-url:https://example.com/file.mender` - Update from a remote URL
- `update-from-url:https://example.com/file.mender#sha256=checksum` - Update from URL with checksum verification

**Examples:**
```bash
# Force an immediate update check on each component
redis-cli LPUSH scooter:update:mdb check-now
redis-cli LPUSH scooter:update:dbc check-now

# Ask both components what switching to stable would cost
redis-cli LPUSH scooter:update:mdb preview-channel:stable
redis-cli LPUSH scooter:update:dbc preview-channel:stable
redis-cli HMGET ota preview-status:mdb preview-version:mdb preview-size:mdb

# Update from a local file (auto-detects checksum if provided)
redis-cli LPUSH scooter:update:dbc "update-from-file:/data/ota/librescoot-unu-dbc-nightly-20251212T024719.mender"

# Update from a URL with checksum verification
redis-cli LPUSH scooter:update:dbc "update-from-url:https://github.com/librescoot/librescoot/releases/download/nightly-20251212T024719/librescoot-unu-dbc-nightly-20251212T024719.mender#sha256=abc123..."

# Update specific component only
redis-cli LPUSH scooter:update:mdb "update-from-file:/data/ota/librescoot-unu-mdb-nightly-20251212T024719.mender"
```

**Auto-Detection:**
- URLs are automatically detected by `http://`, `https://`, or `file://` prefixes
- All other paths are treated as local file paths

**Update Method Selection:**
- The service automatically chooses between delta and full updates based on:
  - The configured update method (`updates.{component}.method` in Redis)
  - Availability of the base Mender file for the current version
  - If delta is configured but no base file exists, falls back to full update

**Checksum Format:**
- Only SHA256 checksums are supported
- Preferred: append `#sha256=<hexdigest>` to the file path or URL (keeps URLs valid; the fragment is stripped before download)
- Legacy `:sha256:<hexdigest>` is still accepted
- Applies only to the `update-from-file` / `update-from-url` commands; scheduled channel updates are not checksum-verified

**Note:** The `check-now` command is useful for:
- Testing update functionality without waiting for the next scheduled check
- Manually checking for updates after deploying new releases
- Forcing an update check after changing update settings

**Note:** Custom update sources are useful for:
- Testing new releases before publishing to GitHub
- Installing updates from alternative sources
- Development and debugging scenarios
- Manual update deployment with specific files

### Channel Previews

`preview-channel:<channel>` answers "what would switching to this channel
fetch" before anything is committed to. It reads the release index for
`<channel>`, resolves the latest release carrying a `.mender` artifact for this
component's `variant_id`, and publishes the tag and artifact size to the `ota`
hash. It changes no setting, starts no download, and never touches the update
status fields, so it is safe to issue while an update is in flight.

The size reported is the full artifact, which is what a channel switch actually
downloads: there is no delta base across channels, so a switch forces a full
update regardless of `updates.{component}.method`.

Each component answers for itself. A consumer that wants the total cost of a
scooter-wide switch asks both and adds the two sizes up. This is what the
dashboard's Settings > System > Updates > Channel entry does before it prompts.

Preview statuses:

| Status        | Meaning                                                              |
|---------------|----------------------------------------------------------------------|
| `checking`    | Request accepted, release index fetch in flight                      |
| `ready`       | A release was found; `preview-version` and `preview-size` are set    |
| `unavailable` | The channel has no release with a `.mender` for this `variant_id`    |
| `error`       | Invalid channel, or the release index could not be fetched in time   |

A preview is bounded at 20 seconds end to end, rather than running the full
retry ladder a background check uses, because a rider is waiting on the answer.
The fields are cleared on service start: a preview from before a restart is not
an answer to anything currently being asked.

## Component-Specific Update Constraints

### DBC Updates
- DBC updates should not turn off the DBC during the update process.
- The vehicle must remain capable of locking and becoming un-drivable during DBC updates.
- Custom update sources work the same way as automatic updates - the service handles power management automatically.

### MDB Updates
- MDB updates can generally be installed at any time the vehicle is not in a critical state.
- MDB reboots should only occur when the scooter is in stand-by mode, managed via the power inhibitor client.
- Custom update sources respect the same reboot constraints as automatic updates.

**Note:** When using custom update sources with the `delta` update method:
- The service requires a base Mender file for the current version to exist in the download directory
- If no base file is found, the update automatically falls back to full update mode
- The service automatically detects the current version from Redis (`version:{component}`)

## Architecture

The Update Service operates as component-specific instances. Each instance includes:

- **Main Application**: Parses flags, sets up logging, and initializes clients.
- **Config**: Holds runtime configuration derived from flags.
- **Redis Client**: Handles all communication with the Redis server for state and messaging.
- **Inhibitor Client**: Communicates with a power management or vehicle state service (via Redis) to request and release update/reboot inhibitions. This ensures updates and reboots only happen at safe times.
- **Updater**:
    - Contains the core logic for the update lifecycle.
    - Fetches release information from the GitHub API.
    - Compares current version with available updates for its assigned component and channel.
    - Manages the download and installation process (interacting with Mender tools via Redis messages).
    - Handles post-installation steps, including reboots, respecting inhibitions.
    - Performs a startup check to commit any pending updates.

## Redis Schema

The Update Service uses Redis to track update state and communicate with other services. All keys are stored in the `ota` hash.

### Status Keys (per component)

| Key                            | Type    | Description                                          | Values                                        |
|--------------------------------|---------|------------------------------------------------------|-----------------------------------------------|
| `status:{component}`           | String  | Current update status                                | `idle`, `downloading`, `installing`, `rebooting`, `error` |
| `update-version:{component}`   | String  | Target version being installed                       | Version string (e.g., `20251009t162327`)      |
| `download-progress:{component}`| Integer | Download progress percentage (0-100)                 | `0` to `100`                                  |
| `download-bytes:{component}`   | Integer | Bytes downloaded so far                              | Byte count (e.g., `12582912`)                 |
| `download-total:{component}`   | Integer | Total download size in bytes                         | Byte count (e.g., `104857600`)                |
| `error:{component}`            | String  | Error type when status is `error`                    | `download-failed`, `checksum-mismatch`, `file-not-found`, `invalid-file`, `image-too-large`, `install-failed`, `no-base-image`, `delta-rejected`, `delta-base-mismatch`, `delta-apply-failed`, `delta-failed`, `reboot-failed` |
| `error-message:{component}`    | String  | Human-readable error message when status is `error`  | Detailed error message                        |
| `preview-channel:{component}`  | String  | Channel the last preview was asked about             | `stable`, `testing`, `nightly`                |
| `preview-status:{component}`   | String  | Outcome of the last preview                          | `checking`, `ready`, `unavailable`, `error`   |
| `preview-version:{component}`  | String  | Release tag the preview resolved to (`ready` only)   | Version string (e.g. `v1.3.0`)                |
| `preview-size:{component}`     | Integer | Size in bytes of the full `.mender` artifact for that release (`ready` only) | Byte count (e.g. `401234432`) |

**Status Meanings:**
- `rebooting`: Update is installed and will be applied on next reboot/power cycle
  - **MDB**: Service waits for vehicle to be in standby for 3 minutes, then actively triggers reboot
  - **DBC**: Update will be applied on next natural power-on (no active reboot triggered)

**Examples:**
- `status:mdb` → `downloading`
- `update-version:mdb` → `20251009t162327`
- `download-progress:mdb` → `45`
- `download-bytes:mdb` → `47185920`
- `download-total:mdb` → `104857600`
- `error:dbc` → `download-failed`
- `error-message:dbc` → `Failed to download update: connection timeout`

**Note:** Error and download progress keys are automatically cleared when:
- The service starts/restarts
- An update completes successfully and status returns to `idle`
- An error occurs (clears download progress only)

## Development

```bash
# Tidy, Format, Test
make tidy fmt test

# Build for host (development)
make host
# This creates ./update-service-host

# Run for MDB component in development (nightly, dry-run)
./update-service-host --component=mdb --channel=nightly --dry-run --check-interval=1m

# Run for DBC component in development (nightly, dry-run)
./update-service-host --component=dbc --channel=nightly --dry-run --check-interval=1m
```

## License

This project is dual-licensed. The source code is available under the
[GNU Affero General Public License v3.0][agpl-3.0].
The maintainers reserve the right to grant separate licenses for commercial distribution; please contact the maintainers to discuss commercial licensing.

[![AGPL v3][agpl-image]][agpl-3.0]

[agpl-3.0]: https://www.gnu.org/licenses/agpl-3.0.en.html
[agpl-image]: https://www.gnu.org/graphics/agplv3-88x31.png
