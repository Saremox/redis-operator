FROM golang:1.26-alpine AS build
WORKDIR /src
COPY writer/ .
RUN CGO_ENABLED=0 go build -o /sentloss .

FROM redis:7.2.12-alpine
COPY --from=build /sentloss /usr/local/bin/sentloss
COPY probe.sh /usr/local/bin/probe.sh
RUN chmod +x /usr/local/bin/probe.sh
