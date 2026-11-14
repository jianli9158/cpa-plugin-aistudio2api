GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
SPIDERMONKEY_VERSION ?= v158.0.1-aistudio2api.1
SPIDERMONKEY_DIR := $(CURDIR)/spidermonkey
LIBWEBP_VERSION ?= v1.6.0-aistudio2api.1
LIBWEBP_DIR := $(CURDIR)/libwebp
ifeq ($(GOOS),darwin)
  EXT := .dylib
  SM_PLATFORM := $(GOOS)-$(GOARCH)
else ifeq ($(GOOS),windows)
  EXT := .dll
  SM_PLATFORM := $(GOOS)-$(GOARCH)
else
  EXT := .so
  ifeq ($(GOARCH),amd64)
    ifeq ($(LINUX_GLIBC),2.28)
      SM_PLATFORM := linux-amd64-glibc228
    else
      SM_PLATFORM := linux-amd64-gnu
    endif
  else
    SM_PLATFORM := $(GOOS)-$(GOARCH)
  endif
endif

ifneq (,$(filter windows-arm64 linux-amd64-glibc228,$(SM_PLATFORM)))
  SM_URL := https://github.com/jianli9158/cpa-plugin-aistudio2api/releases/download/deps-v1/spidermonkey-$(SPIDERMONKEY_VERSION)-$(SM_PLATFORM).tar.gz
  WEBP_URL := https://github.com/jianli9158/cpa-plugin-aistudio2api/releases/download/deps-v1/libwebp-$(LIBWEBP_VERSION)-$(SM_PLATFORM).tar.gz
else
  SM_URL := https://github.com/Mag1cFall/firefox/releases/download/$(SPIDERMONKEY_VERSION)/spidermonkey-$(SPIDERMONKEY_VERSION)-$(SM_PLATFORM).tar.gz
  WEBP_URL := https://github.com/Mag1cFall/libwebp/releases/download/$(LIBWEBP_VERSION)/libwebp-$(LIBWEBP_VERSION)-$(SM_PLATFORM).tar.gz
endif
OUT_DIR := dist/plugins/$(GOOS)/$(GOARCH)
OUT_FILE := $(OUT_DIR)/aistudio2api$(EXT)

.PHONY: all spidermonkey build test install clean

all: build

spidermonkey:
	@if [ ! -d "$(SPIDERMONKEY_DIR)/lib" ]; then \
		mkdir -p "$(SPIDERMONKEY_DIR)"; \
		curl -fsSL "$(SM_URL)" | tar -xz -C "$(SPIDERMONKEY_DIR)"; \
	fi
	@if [ ! -d "$(LIBWEBP_DIR)/lib" ]; then \
		mkdir -p "$(LIBWEBP_DIR)"; \
		curl -fsSL "$(WEBP_URL)" | tar -xz -C "$(LIBWEBP_DIR)"; \
	fi

build: spidermonkey
	mkdir -p $(OUT_DIR)
	CGO_ENABLED=1 \
	CGO_CPPFLAGS="-I$(SPIDERMONKEY_DIR)/include -I$(LIBWEBP_DIR)/include" \
	CGO_LDFLAGS="-L$(SPIDERMONKEY_DIR)/lib -L$(LIBWEBP_DIR)/lib" \
	go build -trimpath -ldflags="-s -w" -buildmode=c-shared -o $(OUT_FILE) ./cmd/aistudio2api-plugin

test: spidermonkey
	export CGO_ENABLED=1 \
		CGO_CPPFLAGS="-I$(SPIDERMONKEY_DIR)/include -I$(LIBWEBP_DIR)/include" \
		CGO_LDFLAGS="-L$(SPIDERMONKEY_DIR)/lib -L$(LIBWEBP_DIR)/lib" && \
	go vet ./... && \
	go test ./internal/aistudio ./internal/api ./internal/app ./internal/config ./internal/cpa ./internal/imagecodec/... ./internal/proxydial ./internal/resample ./internal/waa

install: build
	cp -f $(OUT_FILE) "/Users/jianli/Library/Application Support/com.cpa.gui/cpa-core/plugins/darwin/arm64/aistudio2api.dylib"
clean:
	rm -rf dist
