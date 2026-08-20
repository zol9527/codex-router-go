# Model Router —— 构建入口。所有产物统一落 dist/，仓库根不再放二进制。
# 裸 make 只打印本清单（不猜你想干什么）；版本号默认取 git describe，
# 可用 VERSION=xxx 覆盖。
#
# 替换运行中的二进制必须原子 mv（原地 cp 会被 macOS 代码签名 SIGKILL），
# install-cli / install-app 都遵守这条。

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DIST    := dist
BINARY  := $(DIST)/codex-router
APP_DIR := $(DIST)/Model Router.app

CLI_INSTALL := $(HOME)/bin/codex-router
APP_INSTALL := $(HOME)/Applications/Model Router.app

GOFLAGS_RELEASE := -trimpath
LDFLAGS_RELEASE := -s -w -X main.version=$(VERSION)

.PHONY: help version cli app install-cli install-app test test-app doctor clean

# 默认目标：裸 make = 帮助。每个目标必须显式说出它做什么。
help:
	@echo "Model Router make 目标（产物统一在 dist/，版本 $(VERSION)）："
	@echo "  make cli          编译 CLI → dist/codex-router（版本注入 + 裁剪）"
	@echo "  make app          构建完整 App → dist/Model Router.app（内嵌同版本 Go 二进制）"
	@echo "  make install-cli  原子替换 ~/bin/codex-router（部署，你来执行）"
	@echo "  make install-app  App 换到 ~/Applications（部署；App 在跑会拒绝）"
	@echo "  make test         Go 全量测试"
	@echo "  make test-app     Swift 包测试"
	@echo "  make doctor       用 dist 里的二进制跑体检"
	@echo "  make version      打印版本号"
	@echo "  make clean        清空 dist/"

version:
	@echo $(VERSION)

cli: $(BINARY)

# 源码必须作为依赖列出：无依赖的规则在有旧产物时永不再构建，
# install-cli 会把陈旧二进制原子替换上线（2026-08-20 实发：dist 里
# 残留旧构建，新注册参数 --efforts 部署后不生效，排查到产物过期）。
GO_SOURCES := $(shell find . -name '*.go' -not -path './dist/*' 2>/dev/null)

$(BINARY): $(GO_SOURCES)
	@mkdir -p $(DIST)
	go build $(GOFLAGS_RELEASE) -ldflags '$(LDFLAGS_RELEASE)' -o $@ ./cmd/codex-router
	@echo "built: $@ (version $(VERSION))"

# App 构建脚本自己做 swift build + go build（内嵌 bundle），这里只传
# 版本号与目标目录 —— 产物同样落 dist/。
app:
	MODEL_ROUTER_VERSION=$(VERSION) ./scripts/build-macos-tray-app.sh "$(APP_DIR)"
	@echo "built: $(APP_DIR)"

# 部署类目标是操作者的动作（约定：构建归 make，部署由你亲手执行）。
install-cli: cli
	@mkdir -p $(dir $(CLI_INSTALL))
	mv "$(BINARY)" "$(CLI_INSTALL)"
	@echo "installed CLI: $(CLI_INSTALL) (version $(VERSION))"

install-app: app
	@if pgrep -f "ModelRouterTray" >/dev/null 2>&1; then \
		echo "quit the running app first: osascript -e 'quit app \"Model Router\"'"; exit 1; \
	fi
	rm -rf "$(APP_INSTALL)"
	mv "$(APP_DIR)" "$(APP_INSTALL)"
	@echo "installed app: $(APP_INSTALL) (version $(VERSION))"

test:
	go test ./...

test-app:
	cd apps/macos/ModelRouterTray && swift test

doctor: cli
	$(BINARY) doctor

clean:
	rm -rf $(DIST)
