BINARY_NAME := update-service
BUILD_DIR := bin
GIT_REV := $(shell git describe --tags --always 2>/dev/null)
ifdef GIT_REV
LDFLAGS := -X main.version=$(GIT_REV)
else
LDFLAGS :=
endif
BUILDFLAGS := -tags netgo,osusergo
MAIN := ./cmd/update-service

.PHONY: build build-arm build-host dist clean test fmt deps lint run run-mdb run-dbc run-dev

build:
	mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS) -s -w" $(BUILDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN)

build-arm: build

build-host:
	mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN)

dist: build

clean:
	rm -rf $(BUILD_DIR)

test:
	go test -v ./...

fmt:
	go fmt ./...

deps:
	go mod download && go mod tidy

lint:
	golangci-lint run

run: build-host
	./$(BUILD_DIR)/$(BINARY_NAME)

run-mdb: build-host
	./$(BUILD_DIR)/$(BINARY_NAME) --component=mdb --channel=nightly --dry-run

run-dbc: build-host
	./$(BUILD_DIR)/$(BINARY_NAME) --component=dbc --channel=nightly --dry-run

run-dev: build-host
	./$(BUILD_DIR)/$(BINARY_NAME) -check-interval=1m -default-channel=nightly -dry-run