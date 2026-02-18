VERSION := $(shell cat VERSION)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT)

BIN     := natron
MODULE  := github.com/zette-dev/natron
MAIN    := ./cmd/natron

DIST    := dist

PLATFORMS := \
	darwin/amd64 \
	darwin/arm64 \
	linux/amd64 \
	linux/arm64

.PHONY: build build-all release bump-patch bump-minor bump-major clean test

# Build for the current platform
build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) $(MAIN)

# Cross-compile for all platforms into dist/
build-all: clean
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$$(echo $$platform | cut -d/ -f1); \
		arch=$$(echo $$platform | cut -d/ -f2); \
		output=$(DIST)/$(BIN)_$${os}_$${arch}; \
		echo "Building $$output..."; \
		GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$output $(MAIN); \
	done
	@echo "Built $(VERSION) binaries:"
	@ls -lh $(DIST)/

# Create a GitHub release and upload all binaries
release: build-all
	@echo "Creating GitHub release v$(VERSION)..."
	gh release create v$(VERSION) $(DIST)/$(BIN)_* \
		--title "v$(VERSION)" \
		--notes "Release v$(VERSION)"
	@echo "Released v$(VERSION)"

# Increment patch version: 0.1.0 -> 0.1.1
bump-patch:
	@version=$$(cat VERSION); \
	major=$$(echo $$version | cut -d. -f1); \
	minor=$$(echo $$version | cut -d. -f2); \
	patch=$$(echo $$version | cut -d. -f3); \
	new_patch=$$((patch + 1)); \
	new_version="$$major.$$minor.$$new_patch"; \
	echo $$new_version > VERSION; \
	echo "Bumped $$version -> $$new_version"

# Increment minor version: 0.1.0 -> 0.2.0
bump-minor:
	@version=$$(cat VERSION); \
	major=$$(echo $$version | cut -d. -f1); \
	minor=$$(echo $$version | cut -d. -f2); \
	new_minor=$$((minor + 1)); \
	new_version="$$major.$$new_minor.0"; \
	echo $$new_version > VERSION; \
	echo "Bumped $$version -> $$new_version"

# Increment major version: 0.1.0 -> 1.0.0
bump-major:
	@version=$$(cat VERSION); \
	major=$$(echo $$version | cut -d. -f1); \
	new_major=$$((major + 1)); \
	new_version="$$new_major.0.0"; \
	echo $$new_version > VERSION; \
	echo "Bumped $$version -> $$new_version"

test:
	go test ./...

clean:
	rm -rf $(DIST) $(BIN)
