# Makefile for geoipcheck.
#
# Targets:
#   build            - compile the binary into ./bin
#   test             - run every test layer with -race
#   test-unit        - unit tests only (default `go test`)
#   test-integration - unit + integration tests (-tags=integration)
#   test-e2e         - end-to-end tests through app.Run (-tags=e2e)
#   bench            - run benchmarks
#   fuzz             - run fuzz targets for a short duration
#   lint             - go vet, golangci-lint, buf lint
#   vuln             - govulncheck
#   proto            - regenerate protobuf code with buf
#   k8s              - render the kustomization and validate it
#   tools            - install the pinned tool versions into GOBIN
#                      (tools-lint, tools-vuln, tools-k8s per CI job)
#   docker           - build container image
#
# All test targets use -race; the service is network-facing so data races
# matter more than build time. -race needs cgo and a C compiler; on a
# machine without one, run `make test RACE=`. CI always runs with -race.

GO              ?= go
RACE            ?= -race
BIN_DIR         ?= bin
BINARY          ?= geoipcheck
PKG             := ./...
LDFLAGS         ?= -s -w

# Pinned so local and CI output can't drift. protoc-gen-go follows the
# protobuf version in go.mod.
BUF_VERSION                := v1.73.0
PROTOC_GEN_GO_GRPC_VERSION := v1.6.2
GOLANGCI_LINT_VERSION      := v2.14.0
GOVULNCHECK_VERSION        := v1.8.0
KUSTOMIZE_VERSION          := v5.8.3
KUBECONFORM_VERSION        := v0.8.0
K8S_VERSION                := 1.33.0

.PHONY: build test test-unit test-integration test-e2e bench fuzz lint vuln proto k8s tools tools-lint tools-vuln tools-k8s docker clean

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) ./cmd/geoipcheck

test: test-unit test-integration test-e2e

test-unit:
	$(GO) test $(RACE) -count=1 $(PKG)

test-integration:
	$(GO) test $(RACE) -count=1 -tags=integration ./internal/...

test-e2e:
	$(GO) test $(RACE) -count=1 -tags=e2e ./test/e2e/...

bench:
	$(GO) test -run=^$$ -bench=. -benchmem ./internal/countrycheck/... ./internal/geoip/...

fuzz:
	$(GO) test -run=^$$ -fuzz=FuzzCheck -fuzztime=30s ./internal/countrycheck

lint:
	$(GO) vet -tags=integration $(PKG)
	$(GO) vet -tags=e2e ./test/e2e/...
	golangci-lint run
	buf lint

vuln:
	govulncheck $(PKG)

proto:
	buf generate

k8s:
	kustomize build deploy/k8s | kubeconform -strict -summary -kubernetes-version $(K8S_VERSION) -

tools: tools-lint tools-vuln tools-k8s

tools-lint:
	$(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

tools-vuln:
	$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

tools-k8s:
	$(GO) install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)
	$(GO) install github.com/yannh/kubeconform/cmd/kubeconform@$(KUBECONFORM_VERSION)

docker:
	docker build -t geoipcheck:dev -f deploy/Dockerfile .

clean:
	rm -rf $(BIN_DIR)
