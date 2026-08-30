BUILD_DIR := build
GO        ?= go

.PHONY: all build mcptransformer listfiles test vet clean

all: build

build: mcptransformer listfiles
	cp config.yaml $(BUILD_DIR)/config.yaml

mcptransformer:
	$(GO) build -o $(BUILD_DIR)/mcptransformer ./cmd/mcptransformer

listfiles:
	$(GO) build -o $(BUILD_DIR)/listfiles ./cmd/listfiles

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf $(BUILD_DIR)
