FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
RUN apk --no-cache add \
  bash

WORKDIR /src
COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH VERSION=$VERSION ./scripts/build.sh

FROM gcr.io/distroless/static-debian13:latest@sha256:1a64f46364620465ef9e9a8f5413757dfdf22695e050bbfd7303eeaf17a59383
COPY --from=build /src/bin/redis-operator /usr/local/bin/
COPY --from=build /src/bin/redis-instance /usr/local/bin/
# The chart also runs the operator as 1000:1000.
USER 1000:1000

ENTRYPOINT ["/usr/local/bin/redis-operator"]
