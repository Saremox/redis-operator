FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
RUN apk --no-cache add \
  bash

WORKDIR /src
COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH VERSION=$VERSION ./scripts/build.sh

FROM gcr.io/distroless/static-debian13:latest@sha256:9e0130bec991f471279b70f1630ada11e46dad439ef2949544f96001225329fb
COPY --from=build /src/bin/redis-operator /usr/local/bin/
COPY --from=build /src/bin/redis-instance /usr/local/bin/
# The chart also runs the operator as 1000:1000.
USER 1000:1000

ENTRYPOINT ["/usr/local/bin/redis-operator"]
