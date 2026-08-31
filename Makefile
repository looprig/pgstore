.PHONY: test test-integration fmt fmt-check vet staticcheck gosec vuln secure check build

GO_DIRS := $(shell GOWORK=off go list -f '{{.Dir}}' ./...)
GO_FILES := $(foreach dir,$(GO_DIRS),$(wildcard $(dir)/*.go))

test:
	GOWORK=off go test -race ./...

test-integration:
	GOWORK=off go test -tags integration -race ./...

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@test -z "$$(gofmt -l $(GO_FILES))" || (gofmt -l $(GO_FILES) && exit 1)

vet:
	GOWORK=off go vet ./...

staticcheck:
	GOWORK=off go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...

gosec:
	GOWORK=off go run github.com/securego/gosec/v2/cmd/gosec@v2.28.0 -quiet $(GO_DIRS)

vuln:
	GOWORK=off go mod verify
	GOWORK=off go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...

secure: fmt-check vet staticcheck gosec vuln

build:
	GOWORK=off go build ./...

check: fmt-check vet staticcheck gosec vuln test build
