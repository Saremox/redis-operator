FROM golang:1.26-alpine

RUN apk --no-cache add \
    bash \
    git \
    g++ \
    openssl

# Mock creator for `make mocks` and `make go-generate`. Most committed mocks
# come from v2.20.0, but v2.20.0 panics on the Go 1.26 standard library.
# v2.53.7 works and generated mocks/metrics/Recorder.go.
ARG MOCKERY_VERSION="2.53.7"
RUN wget -c https://github.com/vektra/mockery/releases/download/v${MOCKERY_VERSION}/mockery_${MOCKERY_VERSION}_$(uname -o)_$(uname -m).tar.gz -O - | tar -xz -C /go/bin/

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
