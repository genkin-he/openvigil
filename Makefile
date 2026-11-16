BIN_DIR ?= $(HOME)/.local/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# macOS 按签名记权限（App 管理、辅助功能等）。临时签名每次编译都变，系统就会反复要权限；
# 用钥匙串里固定的证书签，换版本也算同一个程序。证书用 scripts/signing-cert.sh 建一次。
SIGN_ID ?= Vigil Local Signing
# 签名标识跟 launchd 服务名一致（config.yaml 的 launchd_label，没写就是默认的 dev.vigil.daemon）
SIGN_IDENT ?= $(or $(shell sed -n 's/^launchd_label: *//p' $(HOME)/.vigil/config.yaml 2>/dev/null),dev.vigil.daemon)

.PHONY: build test install uninstall release

build:
	go build -ldflags "$(LDFLAGS)" -o bin/vigil ./cmd/vigil

test:
	go vet ./...
	go test -race ./...

# 构建、测试、放到 ~/.local/bin，再装成 launchd 服务（已装过会重启到新版本）
install: test
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/vigil ./cmd/vigil
	@if security find-certificate -c "$(SIGN_ID)" >/dev/null 2>&1; then \
		codesign -f -s "$(SIGN_ID)" -i $(SIGN_IDENT) $(BIN_DIR)/vigil && echo "已用「$(SIGN_ID)」签名，系统权限不会因升级丢失"; \
	else \
		codesign -f -s - -i $(SIGN_IDENT) $(BIN_DIR)/vigil && echo "提示：没有签名证书，每次升级 macOS 都会重新要权限；跑一次 scripts/signing-cert.sh 可解决"; \
	fi
	$(BIN_DIR)/vigil install

uninstall:
	$(BIN_DIR)/vigil uninstall

# macOS 两种架构的发布包和校验和，放在 dist/，上传到 GitHub Release
release: test
	rm -rf dist && mkdir -p dist
	for arch in arm64 amd64; do \
		GOOS=darwin GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/vigil ./cmd/vigil && \
		tar -C dist -czf dist/vigil_darwin_$$arch.tar.gz vigil && rm dist/vigil; \
	done
	cd dist && shasum -a 256 *.tar.gz > checksums.txt
	@ls -l dist
