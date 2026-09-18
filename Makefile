GO ?= docker run --rm -v "$(PWD)":/src -w /src golang:1.24-alpine go

.PHONY: test vet build image run
test:
	$(GO) test ./...
vet:
	$(GO) vet ./...
build:
	$(GO) build ./...

image:
	docker build -t sapedb:latest .

run: image
	docker run --rm -p 7433:7433 \
		-e SAPEDB_SECRET="$${SAPEDB_SECRET:?đặt SAPEDB_SECRET}" \
		-e SAPEDB_INSECURE=1 \
		-v sapedb-data:/var/lib/sapedb \
		sapedb:latest
