# DogeLuckyMining - build binaries for multiple OS/arch
# Usage: make build   (or: make)
# On Windows with Git Bash or WSL: make build

BINARY := dogelucky
DIST   := dist
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

.PHONY: build clean all

build: clean
	@mkdir -p $(DIST)
	GOOS=linux   GOARCH=amd64 go build -ldflags "-s -w" -o $(DIST)/$(BINARY)-linux-amd64 .
	GOOS=linux   GOARCH=arm64 go build -ldflags "-s -w" -o $(DIST)/$(BINARY)-linux-arm64 .
	GOOS=darwin  GOARCH=amd64 go build -ldflags "-s -w" -o $(DIST)/$(BINARY)-darwin-amd64 .
	GOOS=darwin  GOARCH=arm64 go build -ldflags "-s -w" -o $(DIST)/$(BINARY)-darwin-arm64 .
	GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" -o $(DIST)/$(BINARY)-windows-amd64.exe .
	GOOS=windows GOARCH=386   go build -ldflags "-s -w" -o $(DIST)/$(BINARY)-windows-386.exe .
	@echo "Generating SHA256SUMS.txt for verification..."
	@cd $(DIST) && (sha256sum * 2>/dev/null || shasum -a 256 *) > SHA256SUMS.txt
	@echo "Built in $(DIST)/"

clean:
	@rm -rf $(DIST)

all: build
