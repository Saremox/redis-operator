# Build from the repository root, so the replace to the operator module
# resolves: docker build -f test/soak/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN cd test/soak && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -ldflags "-s -w -X main.version=$VERSION" -o /out/soak ./cmd/soak

# operator_upgrade runs helm.
FROM alpine/helm:3.19.0 AS helm

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=build /out/soak /usr/local/bin/soak
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/soak"]
