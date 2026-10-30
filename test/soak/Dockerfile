# Build from the repository root, because go.mod replaces the operator
# module with the root: docker build -f test/soak/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /src
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN cd test/soak && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -ldflags "-s -w -X main.version=$VERSION" -o /out/soak ./cmd/soak

# operator_upgrade runs helm.
FROM alpine/helm:4.3.1@sha256:e3f8d5c462d75470fbb1fce79ad5c20063657096d1f98c0fcea869e2600e2e5e AS helm

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=build /out/soak /usr/local/bin/soak
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/soak"]
