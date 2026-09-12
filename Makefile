# MiBee Eye Makefile
# Cross-compile for SBC (ARM64) from workstation

GOOS ?= linux
GOARCH ?= arm64
BINARY := build/mibee-eye
REMOTE_HOST ?= pi@192.168.1.100
REMOTE_DIR ?= ~/mibee-eye

.PHONY: build test deploy clean service-restart service-stop service-logs service-status mediamtx-disable

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o $(BINARY) ./cmd/server

test:
	go test ./...

deploy: build
	scp $(BINARY) configs/config.yaml $(REMOTE_HOST):$(REMOTE_DIR)/
	scp deploy/mibee-eye.service $(REMOTE_HOST):/tmp/
	ssh $(REMOTE_HOST) 'sudo mv /tmp/mibee-eye.service /etc/systemd/system/ && sudo systemctl daemon-reload && sudo systemctl enable mibee-eye'

service-restart:
	ssh $(REMOTE_HOST) 'sudo systemctl restart mibee-eye'

service-stop:
	ssh $(REMOTE_HOST) 'sudo systemctl stop mibee-eye'

service-logs:
	ssh $(REMOTE_HOST) 'journalctl -u mibee-eye -f'

service-status:
	ssh $(REMOTE_HOST) 'systemctl status mibee-eye'

mediamtx-disable:
	ssh $(REMOTE_HOST) 'sudo systemctl stop mediamtx && sudo systemctl disable mediamtx'

clean:
	rm -rf build/
