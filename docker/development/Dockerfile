FROM golang:1.26-alpine

RUN apk --no-cache add \
    bash \
    git \
    g++ \
    openssl

# Mock creator for `make mocks` and `make go-generate`. The committed mocks
# come from this version. v2.20.0 panics on the Go 1.26 standard library.
ARG MOCKERY_VERSION="2.53.7"
# The checksums are those of the release archives. A changed archive fails the build.
ARG MOCKERY_SHA256_X86_64="a1b0a9191bcab64688b483115293706f841d98048c0f918d7aca89600bba845b"
ARG MOCKERY_SHA256_ARM64="9f95a4cbd1427f4d22dfdf0e9cf3138b72c3f4aedce0cc6fa89e75ba0074b711"
RUN case "$(uname -m)" in \
      x86_64) arch=x86_64; sum="${MOCKERY_SHA256_X86_64}" ;; \
      aarch64|arm64) arch=arm64; sum="${MOCKERY_SHA256_ARM64}" ;; \
      *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;; \
    esac && \
    wget -q -O /tmp/mockery.tar.gz "https://github.com/vektra/mockery/releases/download/v${MOCKERY_VERSION}/mockery_${MOCKERY_VERSION}_Linux_${arch}.tar.gz" && \
    echo "${sum}  /tmp/mockery.tar.gz" | sha256sum -c - && \
    tar -xzf /tmp/mockery.tar.gz -C /go/bin/ mockery && \
    rm /tmp/mockery.tar.gz

# Create user
ARG uid=1000
ARG gid=1000
RUN if ! getent passwd "$uid" > /dev/null 2>&1; then \
      addgroup -g "$gid" rf && \
      adduser -D -u "$uid" -G rf rf; \
    fi && \
    chown -R "$uid":"$gid" /go

USER $uid
WORKDIR /go/src/github.com/saremox/redis-operator
