# whatsapp-bridge container image.
#   docker build -t whatsapp-bridge .
#   docker run --rm whatsapp-bridge keygen
# Self-hosting with Caddy in front: deploy/docker-compose.yml and deploy/SELF-HOSTING.md.

# 1. Web UI. The output is plain files, so it always builds on the build host's
#    platform, into internal/web/assets/dist.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src
COPY web/package.json web/package-lock.json web/
RUN npm --prefix web ci --no-audit --no-fund
COPY web/ web/
RUN npm --prefix web run build

# 2. Go binary. Pure Go (wasm-backed SQLite, no cgo), cross-compiled for the target.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
# The image pins its own Go (GOTOOLCHAIN=local); allow fetching the patch release
# go.mod asks for when a cached image is older.
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/web/assets/dist/ internal/web/assets/dist/
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/whatsapp-bridge ./cmd/whatsapp-bridge \
 && install -d -m 0700 /out/data

# 3. Runtime: static binary, CA certificates, no shell, runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/whatsapp-bridge /whatsapp-bridge
COPY --from=build --chown=65532:65532 /out/data /data
ENV BRIDGE_DATA_DIR=/data BRIDGE_ADDR=0.0.0.0:8080
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/whatsapp-bridge"]
CMD ["serve"]
