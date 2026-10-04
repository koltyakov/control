.PHONY: build test vet check proto test-compose test-compose-webrtc test-compose-relay

.PHONY: bundle update update-status
PLATFORMS ?= linux/amd64,linux/arm64,darwin/amd64,darwin/arm64,windows/amd64,windows/arm64

bundle:
	go run ./cmd/control-bundle --platforms "$(PLATFORMS)" $(if $(VERSION),--version "$(VERSION)",)

update: build bundle
	bin/control update push dist

update-status: build
	bin/control update status

build:
	go run ./cmd/control-bundle --binary bin/control $(if $(VERSION),--version "$(VERSION)",)

test:
	go test -race ./... -timeout=120s

vet:
	go vet ./...

check: vet test

test-compose:
	sh tests/compose/test.sh

test-compose-webrtc:
	sh tests/compose/test.sh webrtc

test-compose-relay:
	sh tests/compose/test.sh relay

# Install protoc and protoc-gen-go before regenerating the checked-in bindings.
proto:
	protoc --go_out=. --go_opt=paths=source_relative internal/protocol/control.proto
