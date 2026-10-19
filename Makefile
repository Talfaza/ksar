# Ksar build helpers.

GO ?= go
AGENT := internal/agentbin/bin/ksar-agent

.PHONY: all ksar agent kernel hello-initramfs boot alpine test clean distclean

all: ksar hello-initramfs

# The agent is embedded into ksar, so build it first.
agent:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' -o $(AGENT) ./cmd/ksar-agent

ksar: agent
	$(GO) build -o ksar ./cmd/ksar

build/hello-init: $(shell find cmd/ksar-hello-init -name '*.go')
	@mkdir -p build
	CGO_ENABLED=0 $(GO) build -o $@ ./cmd/ksar-hello-init

build/hello.cpio: ksar build/hello-init
	./ksar initramfs -init build/hello-init -o $@

hello-initramfs: build/hello.cpio

# The Ksar guest kernel (Linux LTS + kernel/ksar.config) -> build/bzImage.
# ksar run uses it automatically once built.
kernel: build/bzImage
build/bzImage: kernel/ksar.config kernel/build.sh
	./kernel/build.sh

# Boot the host kernel with the hello init (stage 2 demo).
boot: all
	./ksar boot -initrd build/hello.cpio

# Export an Alpine Linux image to use with `ksar run -image`.
alpine: build/alpine.tar
build/alpine.tar:
	@mkdir -p build
	docker pull -q alpine:3.20 >/dev/null
	cid=$$(docker create alpine:3.20) && docker export $$cid -o $@; docker rm $$cid >/dev/null

test: agent
	$(GO) test ./...

# Keeps the downloaded kernel source; `make distclean` removes it too.
clean:
	rm -rf ksar build/bzImage build/*.cpio build/hello-init $(AGENT)

distclean:
	rm -rf ksar build $(AGENT)
