BINARY := admin-server-go
GOFLAGS := -trimpath
LDFLAGS := -s -w
# 只测本仓库的包，避免误扫无关目录。
PKGS := ./cmd/... ./internal/...

.PHONY: all build test race vet fmt check clean run

all: build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/admin-server-go

vet:
	go vet ./...

test:
	go test $(PKGS) -count=1

race:
	go test $(PKGS) -count=1 -race

# 聚合门禁：路由↔文档机检 + vet + 单测。退出码 0 才算过。
check:
	node scripts/check-api-docs.mjs
	go vet ./...
	go test $(PKGS) -count=1

fmt:
	gofmt -l .

run: build
	./$(BINARY)

clean:
	rm -f $(BINARY)
