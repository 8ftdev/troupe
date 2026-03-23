VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)
BINARY  := troupe
DIST    := dist

PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

.PHONY: build clean test lint dist all

all: test lint build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/troupe/

test:
	go test ./... -count=1

lint:
	go vet ./...
	golangci-lint run

clean:
	rm -rf $(BINARY) $(DIST)

dist: clean
	@mkdir -p $(DIST)
	$(foreach platform,$(PLATFORMS),\
		$(eval GOOS := $(word 1,$(subst /, ,$(platform))))\
		$(eval GOARCH := $(word 2,$(subst /, ,$(platform))))\
		$(eval OUT := $(DIST)/$(BINARY)-$(GOOS)-$(GOARCH))\
		GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -o $(OUT) ./cmd/troupe/ && \
	)true
	@echo "Binaries:"
	@ls -lh $(DIST)/
