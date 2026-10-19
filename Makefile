# Ksar build helpers.

GO ?= go

.PHONY: all ksar hello-initramfs boot test clean

all: ksar hello-initramfs

ksar:
	$(GO) build -o ksar ./cmd/ksar

build/hello-init: $(shell find cmd/ksar-hello-init -name '*.go')
	@mkdir -p build
	CGO_ENABLED=0 $(GO) build -o $@ ./cmd/ksar-hello-init

build/hello.cpio: ksar build/hello-init
	./ksar initramfs -init build/hello-init -o $@

hello-initramfs: build/hello.cpio

# Boot the host kernel with the hello init.
boot: all
	./ksar boot -initrd build/hello.cpio

test:
	$(GO) test ./...

clean:
	rm -rf ksar build
