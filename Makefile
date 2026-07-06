# Charger Simulator — slice 1
#
# Targets:
#   make tidy      Fetch dependencies
#   make build     Build worker + stub-cms into ./bin
#   make stub      Run the stub CMS on :9000
#   make sim       Run one virtual charger against the stub CMS
#   make demo      Run both in two consoles (linux/mac); on Windows use 'make stub' and 'make sim' in separate terminals
#   make clean     Remove build output

GOBIN ?= ./bin
WORKER_OUT := $(GOBIN)/worker
STUB_OUT   := $(GOBIN)/stub-cms

.PHONY: tidy build stub sim clean

tidy:
	go mod tidy

build:
	mkdir -p $(GOBIN)
	go build -o $(WORKER_OUT) ./cmd/worker
	go build -o $(STUB_OUT) ./cmd/stub-cms

stub: build
	$(STUB_OUT) --addr :9000

sim: build
	$(WORKER_OUT) \
	  --cms-url ws://localhost:9000/ocpp \
	  --cp-id SIM-CP-0001 \
	  --vendor "TobOR Sim" \
	  --model "Virtual AC 22kW" \
	  --max-kw 22 --phases 3 --voltage 400 \
	  --heartbeat 30s --meter 5s \
	  --plug-in-after 10s --session 60s \
	  --id-tag TESTRFID0001 \
	  --log info

clean:
	rm -rf $(GOBIN)
