BINARY := church-mcp
LDFLAGS := -s -w

.PHONY: dev run test vet build build-linux build-linux-arm clean

# Serve templates/static from disk: edit HTML/JS/CSS and just refresh.
# Port 8150, so the church's other apps (8080 to 8140) can run alongside.
dev:
	DEV=1 go run . -addr 127.0.0.1:8150

run: build
	./bin/$(BINARY)

test:
	go vet ./...
	go test ./...

vet:
	go vet ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) .

# Digital Ocean droplets / most home-lab servers
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY)-linux-amd64 .

# Raspberry Pi 4/5 and other ARM boxes
build-linux-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY)-linux-arm64 .

# Deploying (deploy/ansible, like the other apps) comes in phase 4 of
# ../.claude/plans/mcp-server.md.

clean:
	rm -rf bin
