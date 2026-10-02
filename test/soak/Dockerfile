# Build from the repository root, so the replace to the operator module
# resolves: docker build -f test/soak/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build
WORKDIR /src
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN cd test/soak && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -ldflags "-s -w -X main.version=$VERSION" -o /out/soak ./cmd/soak

# operator_upgrade runs helm.
FROM alpine/helm:3.19.0@sha256:aef9b56f64e866207d9591d0abd8f6d767b36aadd12edf68f8a719716d9d29c9 AS helm

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=build /out/soak /usr/local/bin/soak
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/soak"]
