.PHONY: build test vet lint vuln check clean

# Build both binaries
build:
	go build -o bin/claudecodedocs ./cmd/claudecodedocs
	go build -o bin/snapshotreadme ./cmd/snapshotreadme

# Run tests with race detector
test:
	go test -race -count=1 ./...

# Run go vet
vet:
	go vet ./...

# Run golangci-lint (install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.8)
lint:
	golangci-lint run

# Run govulncheck for known vulnerabilities
vuln:
	govulncheck ./...

# Run all checks (what CI runs)
check: vet test lint vuln

# Remove build artifacts
clean:
	rm -rf bin/
