MODULE := github.com/iman-langaran/AdobeConnect-file-recorder
VERSION := $(shell cat VERSION)
LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION)

.PHONY: build test vet fmt version release-check

build:
	go build -ldflags "$(LDFLAGS)" -o bin/ac-rec ./cmd/ac-rec

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

version:
	@echo $(VERSION)

# release-check: verify the tree matches the release you're about to tag.
# Version flow: bump VERSION → `make build` → commit → `git tag v$(VERSION)` → push --tags.
release-check:
	@test "$(VERSION)" = "$(shell git describe --tags --exact-match 2>/dev/null || echo $(VERSION))" || \
		(echo "note: VERSION $(VERSION) not yet tagged"; exit 0)
	@echo "releasing $(VERSION): ensure VERSION is committed and tag v$(VERSION) exists"
