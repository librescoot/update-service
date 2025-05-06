.PHONY: build clean run test

# Binary name
BINARY_NAME=update-service

# Build the binary
build:
	go build -o $(BINARY_NAME) ./cmd/update-service

# Clean build artifacts
clean:
	go clean
	rm -f $(BINARY_NAME)

# Run the service
run: build
	./$(BINARY_NAME)

# Run with shorter check interval for development
run-dev: build
	./$(BINARY_NAME) -check-interval=1m -default-channel=nightly -dry-run

# Run tests
test:
	go test -v ./...

# Format code
fmt:
	go fmt ./...

# Run go mod tidy
tidy:
	go mod tidy

# Build and install
install: build
	cp $(BINARY_NAME) /usr/local/bin/

# All-in-one setup
all: tidy fmt build test
