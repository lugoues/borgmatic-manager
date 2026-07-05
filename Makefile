VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

PREFIX      ?= /usr/local
BINDIR      := $(PREFIX)/bin
UNITDIR     ?= /etc/systemd/system
CONFDIR     ?= /etc/borgmatic-manager

.PHONY: build test race lint fmt e2e e2e-dind install uninstall clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/borgmatic-manager ./cmd/borgmatic-manager

test:
	go vet ./...
	go test ./...

race:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w cmd internal

e2e: build
	bash tests/integration/e2e_test.sh

# Hermetic variant: manager + borg + borgmatic + test containers all inside
# a privileged docker-in-docker host.
e2e-dind:
	bash tests/integration/dind_test.sh

install: build
	install -D -m 0755 bin/borgmatic-manager $(DESTDIR)$(BINDIR)/borgmatic-manager
	install -D -m 0644 deploy/systemd/borgmatic-manager.service $(DESTDIR)$(UNITDIR)/borgmatic-manager.service
	@if [ ! -f $(DESTDIR)$(CONFDIR)/manager.yaml ]; then \
		install -D -m 0644 config/manager.yaml $(DESTDIR)$(CONFDIR)/manager.yaml; \
	else \
		echo "keeping existing $(DESTDIR)$(CONFDIR)/manager.yaml"; \
	fi
	@echo "installed. next: edit $(CONFDIR)/manager.yaml, then 'systemctl daemon-reload && systemctl enable --now borgmatic-manager'"

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/borgmatic-manager
	rm -f $(DESTDIR)$(UNITDIR)/borgmatic-manager.service

clean:
	rm -rf bin dist
