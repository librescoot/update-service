# Test Plan for update-service

## Executive Summary

The update-service is a moderately complex OTA update orchestrator with **moderate testability challenges**. While the core business logic is testable, the service has significant external dependencies (Redis, HTTP, external commands) that require careful mocking/stubbing strategies.

**Overall Assessment**: The service is testable but requires thoughtful test design. The main challenges stem from:
- Heavy Redis integration throughout (state, pub/sub, settings)
- External command execution (`mender-update`, Python delta script)
- HTTP client interactions with GitHub API
- Complex state machine with timing-dependent behavior (standby waiting)
- Goroutine-based concurrency patterns

## Service Architecture Overview

### Core Components

1. **updater/updater.go** (631 lines): Main orchestrator
   - Update check loop with configurable intervals
   - Component-aware updates (mdb/dbc)
   - Full and delta update workflows
   - Complex MDB reboot logic with standby timing
   - Settings monitoring via Redis pub/sub

2. **updater/github.go** (152 lines): GitHub API client
   - Release fetching with exponential backoff
   - Retry logic with jitter

3. **mender/mender.go** (194 lines): Mender operations manager
   - Download coordination
   - Installation orchestration
   - Delta update application

4. **mender/download.go** (289 lines): HTTP download handling
   - Resumable downloads
   - Progress tracking
   - Checksum verification

5. **mender/install.go** (61 lines): Mender CLI wrapper
   - `mender-update install` execution
   - `mender-update commit` execution

6. **mender/delta.go** (100 lines): Delta update application
   - Calls external Python script
   - File management

7. **redis/client.go** (348 lines): Redis abstraction layer
   - Pub/sub subscriptions
   - Hash operations
   - Vehicle state queries

8. **status/reporter.go** (280 lines): Status reporting
   - Redis status updates
   - Pipeline operations
   - Download progress tracking

9. **config/config.go** (160 lines): Configuration management
   - CLI flags
   - Redis settings loading
   - Runtime updates

10. **inhibitor/client.go** (152 lines): Power inhibit management
11. **power/client.go** (170 lines): Power/governor control

## Testability Analysis

### What's Easy to Test

#### 1. Config Package (`internal/config`)
**Testability: ⭐⭐⭐⭐⭐ (Excellent)**

- Pure logic, minimal dependencies
- Clear input/output behavior
- Interface for Redis (`RedisSettings`)

**Test Focus:**
- Configuration parsing and validation
- Redis settings loading with mock Redis
- CLI flag precedence logic
- Setting update application

**Example test cases:**
```go
func TestNew_CreatesConfigWithDefaults(t *testing.T)
func TestIsValidComponent(t *testing.T)
func TestIsValidChannel(t *testing.T)
func TestLoadFromRedis_LoadsAllSettings(t *testing.T)
func TestLoadFromRedis_HandlesNeverCheckInterval(t *testing.T)
func TestApplyRedisUpdate_ComponentPrefix(t *testing.T)
```

#### 2. Status Reporter (`internal/status`)
**Testability: ⭐⭐⭐⭐ (Good)**

- Well-defined interface with Redis
- Uses redis.Client which can be mocked
- Deterministic pipeline operations

**Challenges:**
- Requires miniredis or Redis mock
- Pipeline behavior needs verification

**Test Focus:**
- Status transitions
- Pipeline atomicity
- Error key management
- Download progress calculations

**Example test cases:**
```go
func TestSetStatus_UpdatesRedisHash(t *testing.T)
func TestSetStatusAndVersion_Atomicity(t *testing.T)
func TestSetIdleAndClearVersion_RemovesAllKeys(t *testing.T)
func TestSetDownloadProgress_CalculatesPercentage(t *testing.T)
func TestInitialize_SetsDefaultKeys(t *testing.T)
```

#### 3. GitHub API Client (`internal/updater/github.go`)
**Testability: ⭐⭐⭐⭐ (Good)**

- HTTP client can be mocked with httptest
- Retry logic is deterministic
- Context-aware

**Challenges:**
- Random jitter in backoff (use fixed seed for tests)
- Context timing

**Test Focus:**
- Successful release fetching
- Retry logic with failures
- Backoff calculation
- Context cancellation
- HTTP error handling

**Example test cases:**
```go
func TestGetReleases_Success(t *testing.T)
func TestGetReleases_RetriesOnFailure(t *testing.T)
func TestGetReleases_ExponentialBackoff(t *testing.T)
func TestGetReleases_ContextCancellation(t *testing.T)
func TestGetReleases_InvalidJSON(t *testing.T)
func TestGetReleases_RateLimitHandling(t *testing.T)
```

### What's Moderately Difficult to Test

#### 4. Mender Download (`internal/mender/download.go`)
**Testability: ⭐⭐⭐ (Moderate)**

- HTTP testing with httptest works well
- File I/O requires temp directories
- Resumable download logic is complex

**Challenges:**
- File system operations
- Large buffer handling
- Progress callback timing
- TLS configuration (certificate time validation bypass)

**Test Focus:**
- Full download flow
- Resume functionality
- Checksum verification
- Progress reporting
- Error handling (network, disk)
- Context cancellation

**Example test cases:**
```go
func TestDownload_CompleteFile(t *testing.T)
func TestDownload_ResumeDownload(t *testing.T)
func TestDownload_ProgressCallback(t *testing.T)
func TestDownload_AlreadyDownloaded(t *testing.T)
func TestVerifyChecksum_SHA256(t *testing.T)
func TestCleanupStaleTmpFiles(t *testing.T)
```

#### 5. Release Finding Logic (`internal/updater/updater.go`)
**Testability: ⭐⭐⭐⭐ (Good)**

Pure logic functions that can be unit tested:
- `findLatestRelease()`
- `findNextRelease()`
- `findDeltaAsset()`
- `findMenderAsset()`
- `isUpdateNeeded()`

**Test Focus:**
- Channel filtering
- Variant matching
- Version comparison
- Asset URL extraction

**Example test cases:**
```go
func TestFindLatestRelease_FiltersChannel(t *testing.T)
func TestFindLatestRelease_MatchesVariant(t *testing.T)
func TestFindNextRelease_Ordering(t *testing.T)
func TestIsUpdateNeeded_VersionComparison(t *testing.T)
func TestFindDeltaAsset_VariantMatching(t *testing.T)
```

### What's Hard to Test

#### 6. Main Updater Orchestration (`internal/updater/updater.go`)
**Testability: ⭐⭐ (Challenging)**

**Key Challenges:**

1. **Goroutine-based architecture**
   - `updateCheckLoop()` runs in background
   - `monitorSettingsChanges()` runs in background
   - Hard to control timing

2. **Complex dependency graph**
   - Requires: Redis client, Mender manager, Inhibitor, Power client, Status reporter, GitHub API
   - All need mocking/stubbing

3. **State machine complexity**
   - Standby waiting logic spans multiple functions
   - Subscription-based vs polling fallback
   - Time-dependent behavior (3-minute standby requirement)

4. **Side effects everywhere**
   - Redis writes
   - Command execution
   - File system operations

**Recommended Approach:**

Create testable sub-components by:
1. Extracting interfaces for all dependencies
2. Using constructor injection
3. Breaking down large methods
4. Using test doubles

**Example interfaces needed:**
```go
type RedisClient interface {
    GetVehicleState(key string) (string, error)
    GetVehicleStateWithTimestamp(key string) (string, time.Time, error)
    SubscribeToVehicleStateChanges(channel string) (<-chan string, func(), error)
    TriggerReboot() error
    GetUpdateMethod(component string) (string, error)
    SubscribeToSettingsChanges(channel string) (<-chan string, func(), error)
    // ... etc
}

type MenderManager interface {
    DownloadAndVerify(ctx context.Context, url, checksum string, callback ProgressCallback) (string, error)
    Install(filePath string) error
    Commit() error
    NeedsCommit() (bool, error)
    ApplyDeltaUpdate(ctx context.Context, deltaURL, currentVersion string, callback ProgressCallback) (string, error)
    FindMenderFileForVersion(version string) (string, bool)
}

type StatusReporter interface {
    SetStatus(ctx context.Context, status Status) error
    SetStatusAndVersion(ctx context.Context, status Status, version string) error
    SetError(ctx context.Context, errorType, message string) error
    // ... etc
}
```

**Test Focus:**
- Update detection logic with mocked GitHub API
- Update workflow state transitions
- Error handling and recovery
- Inhibitor lifecycle
- Settings change handling

**Example test cases:**
```go
func TestCheckForUpdates_NoUpdateAvailable(t *testing.T)
func TestCheckForUpdates_UpdateNeeded(t *testing.T)
func TestPerformUpdate_FullUpdateFlow(t *testing.T)
func TestPerformUpdate_DownloadFailure(t *testing.T)
func TestPerformDeltaUpdate_FallbackToFull(t *testing.T)
func TestMonitorSettingsChanges_AppliesUpdates(t *testing.T)
```

#### 7. Reboot Logic (`TriggerReboot` and helpers)
**Testability: ⭐⭐ (Challenging)**

**Challenges:**
- Complex timing logic (3-minute standby requirement)
- Multiple execution paths (subscription vs polling)
- Recursive retry logic
- Real-time waiting with timers/tickers
- Context cancellation handling

**Recommended Approach:**
- Mock time.Now() and time.After()
- Use fake timers/tickers
- Test state machine transitions separately from timing

**Test Focus:**
- Standby detection
- Timer accuracy
- State change during wait
- Context cancellation
- Dry-run mode

**Example test cases:**
```go
func TestTriggerReboot_MDB_AlreadyInStandby(t *testing.T)
func TestTriggerReboot_MDB_WaitForStandby(t *testing.T)
func TestTriggerReboot_MDB_StateChangesDuringWait(t *testing.T)
func TestTriggerReboot_DBC_NoReboot(t *testing.T)
func TestTriggerReboot_DryRun(t *testing.T)
```

#### 8. External Command Execution
**Testability: ⭐⭐ (Challenging)**

Functions that execute external commands:
- `mender/install.go`: `mender-update install/commit`
- `mender/delta.go`: `mender-apply-delta.py`

**Challenges:**
- Requires mocking `exec.Command`
- No easy way to intercept without refactoring
- Output parsing needs real command output

**Recommended Approach:**

Option 1: Interface-based (cleaner but more work):
```go
type CommandExecutor interface {
    Run(name string, args ...string) (stdout, stderr string, err error)
}
```

Option 2: Test with fake executables:
- Create test scripts in PATH
- Use build tags for test vs production

Option 3: Integration tests only:
- Skip unit tests for these functions
- Test in integration/E2E environment

**Test Focus:**
- Command argument construction
- Output parsing
- Error handling
- Stderr capture

#### 9. Redis Client (`internal/redis/client.go`)
**Testability: ⭐⭐⭐ (Moderate)**

**Challenges:**
- Pub/sub with goroutines
- Panic on channel close (unconventional error handling)
- Real Redis or miniredis required

**Recommended Approach:**
- Use miniredis for unit tests
- Test pub/sub separately from hash operations
- Mock at caller level for updater tests

**Test Focus:**
- Hash operations
- Subscription lifecycle
- Context cancellation
- Error handling

**Example test cases:**
```go
func TestGetVehicleState(t *testing.T)
func TestSubscribeToVehicleStateChanges(t *testing.T)
func TestGetUpdateMethod_DefaultsToFull(t *testing.T)
func TestTriggerReboot_PushesToList(t *testing.T)
```

## Testing Strategy

### 1. Unit Tests (Priority: High)

Start with the easiest, most valuable units:

**Phase 1: Pure Logic (Week 1)**
- `config/config.go` - Full coverage
- Release finding functions in `updater.go`
- `github.go` retry logic

**Phase 2: Well-Isolated Components (Week 2)**
- `status/reporter.go` with miniredis
- `mender/download.go` with httptest
- `inhibitor/client.go` with miniredis
- `power/client.go` with miniredis

**Phase 3: Complex Integration (Week 3-4)**
- `updater.go` main orchestration with mocks
- Reboot logic with fake timers
- Settings monitoring

### 2. Integration Tests (Priority: Medium)

Test real interactions:
- Real Redis (via Docker Compose)
- Real file downloads (to temp dir)
- Mock GitHub API (httptest)
- Fake mender commands (test scripts)

**Example integration test:**
```go
func TestFullUpdateFlow_Integration(t *testing.T) {
    // Start real Redis
    // Mock GitHub API with httptest
    // Use real file system (temp dir)
    // Fake mender-update command
    // Verify full workflow
}
```

### 3. E2E Tests (Priority: Low)

Full system tests in test environment:
- Real Redis
- Real file downloads
- Real mender-update (in VM/container)
- Test both MDB and DBC components

### Test Utilities Needed

```go
// testutil/redis.go
func SetupMiniredis(t *testing.T) (*miniredis.Miniredis, *redis.Client)

// testutil/http.go
func NewGitHubMockServer(releases []Release) *httptest.Server

// testutil/mender.go
func CreateFakeMenderFile(t *testing.T, dir, version string) string

// testutil/time.go
type FakeClock struct { ... }
func (f *FakeClock) Now() time.Time
func (f *FakeClock) After(d time.Duration) <-chan time.Time

// testutil/exec.go
type FakeCommandExecutor struct { ... }
```

## Test Coverage Goals

| Package | Target Coverage | Priority |
|---------|----------------|----------|
| config | 90%+ | High |
| status | 85%+ | High |
| github | 80%+ | High |
| mender/download | 75%+ | Medium |
| mender/mender | 70%+ | Medium |
| redis | 70%+ | Medium |
| updater (logic) | 75%+ | High |
| updater (orchestration) | 60%+ | Medium |
| mender/install | 50%+ (integration) | Low |
| mender/delta | 50%+ (integration) | Low |

## Refactoring Recommendations for Better Testability

### 1. Dependency Injection
```go
// Current: Hard-coded dependencies in New()
func New(ctx context.Context, cfg *config.Config, redisClient *redis.Client, ...) *Updater

// Better: Accept interfaces
func New(ctx context.Context, cfg *config.Config, redis RedisClient, mender MenderManager, ...) *Updater
```

### 2. Extract Time Dependencies
```go
// Add clock field to Updater
type Updater struct {
    // ...
    clock Clock
}

type Clock interface {
    Now() time.Time
    After(d time.Duration) <-chan time.Time
}
```

### 3. Break Down Large Methods
- `performUpdate()` (151 lines) → Extract phases
- `performDeltaUpdate()` (187 lines) → Extract phases
- `TriggerReboot()` (61 lines) → Extract standby waiting
- `waitForStandbyWithSubscription()` (53 lines) → Simplify

### 4. Command Execution Abstraction
```go
type Installer struct {
    logger *log.Logger
    exec   CommandExecutor  // NEW
}

type CommandExecutor interface {
    Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error)
}
```

### 5. Separate Business Logic from I/O
```go
// Business logic (pure, easy to test)
func (u *Updater) shouldUpdateToRelease(release Release, currentVersion string) bool

// I/O wrapper (integration test)
func (u *Updater) performUpdate(release Release, assetURL string)
```

## Known Testability Issues

### 1. Panic on Redis Channel Close
**Location:** `redis/client.go:106, 222, 288`

```go
if !ok {
    // Channel closed - Redis connection lost
    panic(fmt.Sprintf("Redis channel %s closed unexpectedly..."))
}
```

**Impact:** Makes testing subscription failures difficult
**Recommendation:** Return error instead of panic, let caller decide

### 2. Global Random State
**Location:** `github.go:123`

```go
jitter := 1.0 + (rand.Float64()*2-1.0)*backoffJitter
```

**Impact:** Non-deterministic tests
**Recommendation:** Accept rand.Source in constructor for tests

### 3. Time.Sleep in Tight Loops
**Location:** Multiple places (standby waiting, retry logic)

**Impact:** Slow tests
**Recommendation:** Use ticker channels or fake clock

### 4. Goroutine Leaks
**Location:** Background goroutines in `Start()`, subscriptions

**Impact:** Race conditions in tests
**Recommendation:** Always cleanup with context/defer, add WaitGroup for test synchronization

## Difficulty Rating Summary

| Component | Testability | Reason |
|-----------|------------|---------|
| config | ⭐⭐⭐⭐⭐ | Pure logic, good interfaces |
| status | ⭐⭐⭐⭐ | Clear interface, needs miniredis |
| github | ⭐⭐⭐⭐ | HTTP mocking works well |
| mender/download | ⭐⭐⭐ | File I/O, complex resume logic |
| mender/manager | ⭐⭐⭐ | Coordinator, mockable |
| redis | ⭐⭐⭐ | Needs miniredis, pub/sub complexity |
| inhibitor | ⭐⭐⭐ | Redis operations, straightforward |
| power | ⭐⭐⭐ | Redis operations, straightforward |
| updater (logic) | ⭐⭐⭐⭐ | Pure functions |
| updater (orchestration) | ⭐⭐ | Many dependencies, goroutines, timing |
| mender/install | ⭐⭐ | External commands |
| mender/delta | ⭐⭐ | External commands |
| Reboot logic | ⭐⭐ | Complex timing, state machine |

## Recommended Testing Order

1. **Start here** (easy wins, high value):
   - config package
   - github API retry logic
   - Release finding functions

2. **Next** (moderate effort, high value):
   - status reporter
   - mender download (without resume)
   - Helper functions in updater

3. **Then** (higher effort, necessary):
   - Redis client basics
   - Download resume logic
   - Updater orchestration (with heavy mocking)

4. **Finally** (integration/E2E):
   - Command execution tests
   - Full update workflow
   - Timing-dependent reboot logic

## Conclusion

The update-service is **moderately difficult to test** but not impossible. The main challenges are:

1. **External dependencies** - Redis, HTTP, commands require mocking
2. **Goroutine concurrency** - Background operations complicate synchronization
3. **Timing logic** - Standby waiting and retry logic need careful test design
4. **Large functions** - Some orchestration methods are too large and complex

**Recommendation:** Start with unit tests for pure logic and well-isolated components. Gradually build up to integration tests for the orchestration layer. Consider refactoring suggestions to improve testability before writing comprehensive tests.

With proper interfaces, mocks, and test utilities, you can achieve good test coverage (70-80% overall). Some components (external commands, complex timing) may be better suited for integration/E2E tests rather than unit tests.
