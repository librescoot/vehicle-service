.PHONY: build clean build-arm build-host dist fmt deps lint test

BINARY_NAME=vehicle-service
BUILD_DIR=bin
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-w -s -X main.version=$(VERSION) -extldflags '-static'"

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/vehicle-service

clean:
	rm -rf $(BUILD_DIR)

build-arm: build

build-host:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-host ./cmd/vehicle-service

dist: build

fmt:
	go fmt ./...

deps:
	go mod download && go mod tidy

lint:
	golangci-lint run

test:
	go test -v ./...
