# Update Service

A service for managing over-the-air (OTA) updates for Librescoot vehicles.

## Overview

The Update Service is responsible for:

- Checking a configurable endpoint (GitHub Releases API) for available updates
- Distinguishing between components (e.g., mdb, dbc) and channels (stable, testing, nightly)
- Orchestrating vehicle-service and smut (Simple Mender Update Tool) to download & install updates
- Tracking download & installation progress
- Rebooting affected systems if necessary, with specific constraints

## Features

- GitHub Releases API integration for update discovery
- Component-specific update handling (DBC, MDB)
- Safe update application with vehicle state management
- Controlled reboot scheduling based on vehicle state
- Redis-based communication with vehicle-service and SMUT

## Installation

```bash
# Clone the repository
git clone https://github.com/librescoot/update-service.git
cd update-service

# Build the service
go build -o update-service ./cmd/update-service

# Install as a system service (requires root)
sudo make install
sudo cp librescoot-update-service.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable librescoot-update-service
sudo systemctl start librescoot-update-service
```

## Usage

```bash
# Run with default settings
./update-service

# Run with custom settings
./update-service \
  --redis-addr=localhost:6379 \
  --github-releases-url=https://api.github.com/repos/librescoot/librescoot/releases \
  --check-interval=1h \
  --default-channel=stable \
  --components=dbc,mdb
```

## Configuration

The service can be configured using command-line flags:

| Flag | Description | Default |
|------|-------------|---------|
| `--redis-addr` | Redis server address | `localhost:6379` |
| `--github-releases-url` | GitHub Releases API URL | `https://api.github.com/repos/librescoot/librescoot/releases` |
| `--check-interval` | Interval between update checks | `1h` |
| `--default-channel` | Default update channel | `stable` |
| `--components` | Comma-separated list of components to check for updates | `dbc,mdb` |
| `--dbc-update-key` | Redis key for DBC update URLs | `mender/update/dbc/url` |
| `--mdb-update-key` | Redis key for MDB update URLs | `mender/update/mdb/url` |
| `--dbc-checksum-key` | Redis key for DBC update checksums | `mender/update/dbc/checksum` |
| `--mdb-checksum-key` | Redis key for MDB update checksums | `mender/update/mdb/checksum` |
| `--ota-status-hash-key` | Redis hash key for OTA status | `ota` |
| `--vehicle-hash-key` | Redis hash key for vehicle state | `vehicle` |

## Component-Specific Update Constraints

### DBC Updates
- DBC updates should not turn off the DBC during updates
- The vehicle should still be able to lock and become un-drivable during DBC updates

### MDB Updates
- MDB updates can be installed at any time
- MDB reboots should only happen when the scooter is in stand-by mode

## Architecture

The service follows a modular architecture with clear separation of concerns:

- **Config**: Manages configuration for update endpoints, Redis connection, etc.
- **Redis Client**: Handles all Redis communication
- **Vehicle Service Client**: Interfaces with the vehicle-service via Redis
- **GitHub API Client**: Fetches and parses releases from the GitHub Releases API
- **Updater**: Orchestrates the update process

## Development

```bash
# Run tests
go test ./...

# Build for development
go build -o update-service ./cmd/update-service

# Run with verbose logging
./update-service --check-interval=1m
```

## License

[Affero GPL 3.0](LICENSE.md)
