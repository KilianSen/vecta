# syntax=docker/dockerfile:1
# vecta image: gateway (default) or agent.
#   docker build -t vecta:latest .
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     --build-arg VERSION=1.0.0 -t registry.example.com/vecta:1.0.0 --push .
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
      go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/vecta ./cmd/vecta \
    && mkdir -p /out/data

# Static binary on distroless: no shell, runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/vecta /usr/local/bin/vecta
COPY --from=build --chown=65532:65532 /out/data /data
# The default dataFile (vecta-state.json) lands in the /data volume.
WORKDIR /data
VOLUME /data
EXPOSE 25565 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/vecta", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/vecta"]
CMD ["gateway", "-config", "/etc/vecta/gateway.json"]
