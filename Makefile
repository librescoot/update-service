BIN := update-service
GIT_REV := $(shell git describe --tags --always 2>/dev/null)
ifdef GIT_REV
LDFLAGS := -X main.version=$(GIT_REV)
else
LDFLAGS :=
endif
BUILDFLAGS := -tags netgo,osusergo
MAIN := ./cmd/update-service

.PHONY: build host arm amd64 dist clean test fmt tidy all run run-dev

build: host
host:
	go build -ldflags "$(LDFLAGS)" -o ${BIN}-host ${MAIN}

amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" $(BUILDFLAGS) -o ${BIN}-amd64 ${MAIN}

arm:
	GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" $(BUILDFLAGS) -o ${BIN}-arm ${MAIN}

dist:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS) -s -w" $(BUILDFLAGS) -o ${BIN}-arm-dist ${MAIN}

clean:
	rm -f ${BIN} ${BIN}-host ${BIN}-amd64 ${BIN}-arm ${BIN}-arm-dist

# Run the service
run: host
	./${BIN}-host

# Run with shorter check interval for development
run-dev: host
	./${BIN}-host -check-interval=1m -default-channel=nightly -dry-run

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