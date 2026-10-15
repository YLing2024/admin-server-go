BINARY := admin-server-go
GOFLAGS := -trimpath
LDFLAGS := -s -w
# 只测本仓库的包，避免误扫无关目录。tools 下的迁移工具也纳入。
PKGS := ./cmd/... ./internal/... ./tools/...

.PHONY: all build test race vet fmt check clean run

all: build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/admin-server-go

vet:
	go vet ./...

# 单测只跑 MySQL：由脚本起临时 8.4 容器（绑回环高位端口），验完删除。
test:
	bash scripts/test-mysql.sh

race:
	bash scripts/test-mysql.sh -race

# 聚合门禁：路由↔文档机检 + vet + 单测（临时 MySQL）。退出码 0 才算过。
check:
	node scripts/check-api-docs.mjs
	go vet ./...
	bash scripts/test-mysql.sh

fmt:
	gofmt -l .

run: build
	./$(BINARY)

clean:
	rm -f $(BINARY)
