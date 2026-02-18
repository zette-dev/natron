VERSION := $(shell cat VERSION)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT)

BIN     := natron
MODULE  := github.com/zette-dev/natron
MAIN    := ./cmd/natron

DIST    := dist
TAP_REPO := git@github.com:zette-dev/homebrew-tap.git
TAP_DIR  := .homebrew-tap

PLATFORMS := \
	darwin/amd64 \
	darwin/arm64 \
	linux/amd64 \
	linux/arm64

.PHONY: build build-all release update-tap bump-patch bump-minor bump-major clean test

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

# Tag HEAD, publish GitHub release, and update the Homebrew tap
release: build-all
	@echo "Tagging v$(VERSION) at $(COMMIT)..."
	git tag -a v$(VERSION) -m "Release v$(VERSION)"
	git push origin v$(VERSION)
	@echo "Creating GitHub release v$(VERSION)..."
	gh release create v$(VERSION) $(DIST)/$(BIN)_* \
		--title "v$(VERSION)" \
		--notes "Release v$(VERSION)"
	@$(MAKE) update-tap
	@echo "Released v$(VERSION)"

# Clone the tap repo, regenerate the formula from the template, and push
update-tap:
	@echo "Updating Homebrew tap..."
	@rm -rf $(TAP_DIR)
	@git clone --quiet $(TAP_REPO) $(TAP_DIR)
	@mkdir -p $(TAP_DIR)/Formula
	@SHA_ARM64=$$(shasum -a 256 $(DIST)/$(BIN)_darwin_arm64 | awk '{print $$1}'); \
	SHA_AMD64=$$(shasum -a 256 $(DIST)/$(BIN)_darwin_amd64 | awk '{print $$1}'); \
	sed \
		-e "s/{{VERSION}}/$(VERSION)/g" \
		-e "s/{{SHA_ARM64}}/$$SHA_ARM64/g" \
		-e "s/{{SHA_AMD64}}/$$SHA_AMD64/g" \
		Formula/natron.rb.tmpl > $(TAP_DIR)/Formula/natron.rb
	@cd $(TAP_DIR) && git add Formula/natron.rb && \
		git commit -m "natron $(VERSION)" && \
		git push --quiet
	@rm -rf $(TAP_DIR)
	@echo "Tap updated"

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
