# Multi-stage: satu build image → empat target akhir.
# docker-compose sudah pilih target per service.

FROM golang:1.23-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/axis-core       ./cmd/axis-core
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/register-worker ./cmd/register-worker
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gift-worker     ./cmd/gift-worker
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/axis-ui         ./cmd/axis-ui

FROM alpine:3.20 AS base
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Jakarta

FROM base AS axis-core
COPY --from=build /out/axis-core /usr/local/bin/axis-core
EXPOSE 1213
ENTRYPOINT ["/usr/local/bin/axis-core"]

FROM base AS register-worker
COPY --from=build /out/register-worker /usr/local/bin/register-worker
ENTRYPOINT ["/usr/local/bin/register-worker"]

FROM base AS gift-worker
COPY --from=build /out/gift-worker /usr/local/bin/gift-worker
ENTRYPOINT ["/usr/local/bin/gift-worker"]

FROM base AS axis-ui
COPY --from=build /out/axis-ui /usr/local/bin/axis-ui
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/axis-ui"]
