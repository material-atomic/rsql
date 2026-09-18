# Build in one image, ship in nothing.
#
# The result holds the binary, an empty data directory owned by a non-root
# user, and not one thing else — no shell, no package manager, no libc. A
# database server is exactly the process somebody who gets in wants a shell
# from, and the cheapest way to not give them one is to not ship one.
FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY internal ./internal
COPY cmd ./cmd

# Static: the runtime image has no dynamic loader to find a library with.
# Stripped: the symbol table is of no use in production and is of some use to
# whoever is reading the binary.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /rsqld ./cmd/rsqld

# The data directory is made here, with its ownership, because the runtime
# image has no shell to make one in.
RUN mkdir -p /data && chown 65532:65532 /data

FROM scratch

COPY --from=build /rsqld /rsqld
COPY --from=build --chown=65532:65532 /data /var/lib/rsql

# Numeric, because there is no /etc/passwd here to hold a name. 65532 is the
# convention distroless uses for "nonroot".
USER 65532:65532

# Databases live here and must outlast the container.
VOLUME ["/var/lib/rsql"]

EXPOSE 7433

# No HEALTHCHECK on purpose. A check this image could run without credentials
# only proves the port accepts, which an orchestrator's TCP probe already does;
# a check that proves a database is answering needs a signed connection string,
# which does not belong baked into an image. A health check that passes while
# the thing it checks is broken is worse than none.
ENTRYPOINT ["/rsqld"]
