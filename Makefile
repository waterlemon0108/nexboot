BIN := bin/ndiskless
GOOS ?= linux
GOARCH ?= $(shell go env GOARCH)

# 每个二进制都带上它来自哪个提交。裸 `go build` 出来的是 "dev"——那种二进制
# 对不上任何提交，报障时「你装的什么版本」就答不上来。--dirty 后缀故意保留：
# 本地未提交的改动构建出的东西，必须能和干净构建区分开。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build dev web gen migrate test test-web e2e verify fmt-check vet build-static verify-web verify-all deb offline

# Go embeds web/dist; share this prerequisite even under make -j.
build dev migrate test test-web vet build-static: web

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath $(LDFLAGS) -o $(BIN) ./cmd/ndiskless

dev:
	CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o bin/ndiskless-dev ./cmd/ndiskless

web:
	npm --prefix web ci
	npm --prefix web run build

gen:
	@echo "code generation is implemented in issue 002"

migrate:
	go run ./cmd/ndiskless -migrate-only

test:
	go test ./...

# Frontend behaviour tests (vitest + jsdom), using the same web build.
test-web:
	npm --prefix web run test

# End-to-end matrices against a deployed server. Needs ND_BASE/ND_PASSWORD and
# a real Windows source image; see tools/e2e/README.md. Never point it at a
# production pool — it creates and destroys its own images and groups.
e2e:
	python3 tools/e2e/config_reduction_matrix.py
	python3 tools/e2e/boot_matrix.py
	python3 tools/e2e/adaptation_matrix.py
	python3 tools/e2e/driver_matrix.py
	python3 tools/e2e/platform_matrix.py
	python3 tools/e2e/pool_matrix.py

# ---------------------------------------------------------------- 质量门
#
# verify 是 CI 跑的那一条，也是提交前该在本地跑的那一条——两边同一个入口，
# 免得出现「本地过了、CI 挂了」或者反过来。任何一步失败就整体失败。

fmt-check:
	@out=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$out" ]; then echo "gofmt 未格式化:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

# 静态二进制：产品就是这么交付的（单文件、无 CGO），构建不过等于交付不了。
build-static:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath $(LDFLAGS) -o bin/ndiskless-linux-amd64 ./cmd/ndiskless

# deb：把静态二进制打成 .deb（放文件、声明依赖、装 configure 命令；不启动服务）。
# dpkg-deb 只在 Linux 有，本机（mac）跑不了——在 Ubuntu 或 CI 上执行。
deb: build-static
	VERSION=$(VERSION) BINARY=bin/ndiskless-linux-amd64 packaging/build-deb.sh

# offline：把 deb 连同它的依赖 deb 全抓齐，打成断网机房可直接装的 tar.gz。
# 必须在与目标同版本的 Ubuntu 上、且这台能联网时跑（依赖包按发行版拉取）。
offline: deb
	DEB=$$(ls -t dist/ndiskless_*.deb | head -1) packaging/offline-bundle.sh

verify: fmt-check vet test build-static
	@echo "  后端质量门通过（gofmt / vet / go test / 静态构建）"

verify-web: test-web
	@echo "  前端质量门通过（vitest / vite build）"

verify-all: verify verify-web
