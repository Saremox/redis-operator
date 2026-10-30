FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
RUN apk --no-cache add \
  bash

WORKDIR /src
COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH VERSION=$VERSION ./scripts/build.sh

FROM gcr.io/distroless/static-debian13:latest@sha256:15f8637509792e05b97d83451b0b6ccfa4abd548f997e406ac1e72c87fe1fa78
COPY --from=build /src/bin/redis-operator /usr/local/bin/
COPY --from=build /src/bin/redis-instance /usr/local/bin/
# The chart also runs the operator as 1000:1000.
USER 1000:1000

ENTRYPOINT ["/usr/local/bin/redis-operator"]
