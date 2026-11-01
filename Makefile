# PythonBPF playground
#   make kernel    guest kernel   -> guest/out/vmlinux          (Docker)
#   make rootfs    guest rootfs   -> guest/out/rootfs.squashfs  (Docker)
#   make pgd       server binary  -> server/pgd (static, linux/amd64)
#   make test      unit tests
#   make dev       run locally: firecracker straight from $FIRECRACKER, no jailer

FIRECRACKER ?= $(shell command -v firecracker || echo ./dist/firecracker)
GOARCH ?= amd64

.PHONY: all kernel rootfs pgd test dev static-examples
all: kernel rootfs pgd

kernel:
	guest/kernel/build-kernel.sh

rootfs:
	guest/rootfs/build-rootfs.sh

static-examples:
	rm -rf server/static/examples
	cp -r examples server/static/examples

pgd: static-examples
	cd server && CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o pgd .

test: static-examples
	cd server && go vet ./... && go test -race -count=1 ./...

dev: pgd
	server/pgd serve -direct $(FIRECRACKER) -kernel guest/out/vmlinux -rootfs guest/out/rootfs.squashfs \
	  -trust-cf=false -per-ip=false
