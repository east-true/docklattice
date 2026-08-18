# Disposable Appendix A runner. This image is not a Dockpilot release image.
FROM golang:1.26.5-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/transport-prototype ./cmd/transport-prototype

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates iproute2 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/transport-prototype /usr/local/bin/transport-prototype
COPY prototype/entrypoint.sh /usr/local/bin/prototype-entrypoint
RUN chmod 0755 /usr/local/bin/prototype-entrypoint
ENTRYPOINT ["/usr/local/bin/prototype-entrypoint"]
