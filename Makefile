GO ?= docker run --rm -v "$(PWD)":/src -w /src golang:1.24-alpine go

.PHONY: test vet build image run
test:
	$(GO) test ./...
vet:
	$(GO) vet ./...
build:
	$(GO) build ./...

image:
	docker build -t rsql:latest .

run: image
	docker run --rm -p 7433:7433 \
		-e RSQL_SECRET="$${RSQL_SECRET:?đặt RSQL_SECRET}" \
		-e RSQL_INSECURE=1 \
		-v rsql-data:/var/lib/rsql \
		rsql:latest
