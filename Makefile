BINARY ?= nginx_updata_config
GO ?= go
VERSION ?= dev
DIST_DIR ?= dist
ARCH ?= amd64
PKG_NAME = $(BINARY)-linux-$(ARCH)-$(VERSION)
PKG_ROOT = $(DIST_DIR)/$(PKG_NAME)

.PHONY: all test dist-linux dist-linux-amd64 dist-linux-arm64 _dist
all: test

test:
	$(GO) test ./...
	python3 -m unittest discover -s scripts -p '*_test.py'

dist-linux: dist-linux-amd64
dist-linux-amd64:
	$(MAKE) _dist ARCH=amd64
dist-linux-arm64:
	$(MAKE) _dist ARCH=arm64
_dist:
	mkdir -p $(PKG_ROOT)/bin $(PKG_ROOT)/configs $(PKG_ROOT)/scripts $(PKG_ROOT)/deploy $(PKG_ROOT)/docs
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) $(GO) build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o $(PKG_ROOT)/bin/$(BINARY) ./cmd/nginx_updata_config
	cp configs/service.example.yaml configs/service.advanced.example.yaml configs/frontend-service.example.yaml configs/frontend-location.example.conf configs/prometheus-*.yml $(PKG_ROOT)/configs/
	cp scripts/release-apply.sh scripts/release_http.py scripts/frontend-manifest.py scripts/frontend-artifact.sh $(PKG_ROOT)/scripts/
	cp deploy/nginx-updata-config.service $(PKG_ROOT)/deploy/
	cp docs/frontend-oras.md docs/request-targets.md docs/jenkins.md $(PKG_ROOT)/docs/
	cp Jenkinsfile README.md edge-sync-agent-design-v3.md $(PKG_ROOT)/
	printf '%s\n' 'HTTP release contract 2 — $(VERSION) linux/$(ARCH)' > $(PKG_ROOT)/VERSION
	tar -C $(DIST_DIR) -czf $(DIST_DIR)/$(PKG_NAME).tar.gz $(PKG_NAME)
