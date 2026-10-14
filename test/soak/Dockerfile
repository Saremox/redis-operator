# Build from the repository root, because go.mod replaces the operator
# module with the root: docker build -f test/soak/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
WORKDIR /src
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN cd test/soak && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -ldflags "-s -w -X main.version=$VERSION" -o /out/soak ./cmd/soak

# operator_upgrade runs helm.
FROM alpine/helm:4.3.0@sha256:a6cf54599ccb99d90cf0712b30f03fdb3cab062e6b94e0418cc4db7e8a1464b2 AS helm

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=build /out/soak /usr/local/bin/soak
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/soak"]
