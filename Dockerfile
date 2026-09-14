# syntax=docker/dockerfile:1
# anymcp image: gateway (default) or agent.
#   docker build -t anymcp:latest .
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     --build-arg VERSION=1.0.0 -t registry.example.com/anymcp:1.0.0 --push .
ARG GO_VERSION=1.27

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/anymcp ./cmd/anymcp \
    && mkdir -p /out/data

# Static binary on distroless: no shell, runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/anymcp /usr/local/bin/anymcp
COPY --from=build --chown=65532:65532 /out/data /data
# The default dataFile (anymcp-state.json) lands in the /data volume.
WORKDIR /data
VOLUME /data
EXPOSE 25565 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/anymcp", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/anymcp"]
CMD ["gateway", "-config", "/etc/anymcp/gateway.json"]
