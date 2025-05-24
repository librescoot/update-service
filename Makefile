SERVICES := update-service update-fetcher update-installer
GIT_REV := $(shell git describe --tags --always 2>/dev/null)
ifdef GIT_REV
LDFLAGS := -X main.Version=$(GIT_REV)
else
LDFLAGS :=
endif
BUILDFLAGS := -tags netgo,osusergo

.PHONY: build host arm amd64 dist clean test fmt tidy all run run-dev

build: host

host: $(SERVICES:%=%-host)

amd64: $(SERVICES:%=%-amd64)

arm: $(SERVICES:%=%-arm)

dist: $(SERVICES:%=%-arm-dist)

%-host:
	go build -ldflags "$(LDFLAGS)" -o $@ ./cmd/$(subst -host,,$@)

%-amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" $(BUILDFLAGS) -o $@ ./cmd/$(subst -amd64,,$@)

%-arm:
	GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" $(BUILDFLAGS) -o $@ ./cmd/$(subst -arm,,$@)

%-arm-dist:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS) -s -w" $(BUILDFLAGS) -o $@ ./cmd/$(subst -arm-dist,,$@)

clean:
	rm -f $(SERVICES) $(SERVICES:%=%-host) $(SERVICES:%=%-amd64) $(SERVICES:%=%-arm) $(SERVICES:%=%-arm-dist)

# Run the service
run: host
	./update-service-host

# Run with shorter check interval for development
run-dev: host
	./update-service-host -check-interval=1m -default-channel=nightly -dry-run

# Run fetcher service
run-fetcher: host
	./update-fetcher-host

# Run installer service  
run-installer: host
	./update-installer-host -update-key=mender/update/mdb/url -component=mdb

# Run tests
test:
	go test -v ./...

# Format code
fmt:
	go fmt ./...

# Run go mod tidy
tidy:
	go mod tidy

# All-in-one setup
all: tidy fmt test arm amd64