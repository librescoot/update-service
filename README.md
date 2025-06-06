# Update Service

A service for managing over-the-air (OTA) updates for Librescoot vehicles.

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

The service is configured using command-line flags:

| Flag                    | Description                                                       | Default                                                        | Required |
|-------------------------|-------------------------------------------------------------------|----------------------------------------------------------------|----------|
| `--component`           | Component to manage updates for.                                  | `""`                                                           | **Yes** (must be `mdb` or `dbc`) |
| `--channel`             | Update channel to track.                                          | `nightly`                                                      | No       |
| `--redis-addr`          | Redis server address.                                             | `localhost:6379`                                               | No       |
| `--github-releases-url` | GitHub Releases API URL for update discovery.                     | `https://api.github.com/repos/librescoot/librescoot/releases`  | No       |
| `--check-interval`      | Interval between update checks.                                   | `1h`                                                           | No       |
| `--dry-run`             | If true, log reboot actions instead of performing them.           | `false`                                                        | No       |

Many previous Redis key configurations are now handled internally based on the specified `--component`.

## Component-Specific Update Constraints

### DBC Updates
- DBC updates should not turn off the DBC during the update process.
- The vehicle must remain capable of locking and becoming un-drivable during DBC updates.

### MDB Updates
- MDB updates can generally be installed at any time the vehicle is not in a critical state.
- MDB reboots should only occur when the scooter is in stand-by mode, managed via the power inhibitor client.

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

[Affero GPL 3.0](LICENSE.md)
