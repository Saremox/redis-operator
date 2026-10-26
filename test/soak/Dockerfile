# Build from the repository root, because go.mod replaces the operator
# module with the root: docker build -f test/soak/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
WORKDIR /src
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN cd test/soak && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -ldflags "-s -w -X main.version=$VERSION" -o /out/soak ./cmd/soak

# operator_upgrade runs helm.
FROM alpine/helm:4.3.1@sha256:e3f8d5c462d75470fbb1fce79ad5c20063657096d1f98c0fcea869e2600e2e5e AS helm

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=build /out/soak /usr/local/bin/soak
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/soak"]
