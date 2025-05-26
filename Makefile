BIN := update-service
GIT_REV := $(shell git describe --tags --always 2>/dev/null)
ifdef GIT_REV
LDFLAGS := -X main.version=$(GIT_REV)
else
LDFLAGS :=
endif
BUILDFLAGS := -tags netgo,osusergo
MAIN := ./cmd/update-service

.PHONY: build host arm amd64 dist clean test fmt tidy all run run-mdb run-dbc run-dev install install-template

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

# Run with component flag for new architecture (MDB)
run-mdb: host
	./${BIN}-host --component=mdb --channel=nightly --dry-run

# Run with component flag for new architecture (DBC)  
run-dbc: host
	./${BIN}-host --component=dbc --channel=nightly --dry-run

# Run with shorter check interval for development (legacy)
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

# Install binary and systemd template (requires sudo)
install: dist
	sudo cp ${BIN}-arm-dist /usr/bin/update-service
	sudo chmod +x /usr/bin/update-service

# Install systemd template (requires sudo)
install-template:
	sudo cp librescoot-update@.service /etc/systemd/system/
	sudo systemctl daemon-reload

# Install everything (binary + template)
install-all: install install-template

# All-in-one setup
all: tidy fmt test arm amd64