FROM golang:1.26-alpine AS build

WORKDIR /src

ENV CGO_ENABLED=0 GOOS=linux

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM alpine:3 AS runtime

RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 podium \
    && adduser -u 10001 -G podium -D -H -s /sbin/nologin podium

COPY --from=build /out/ /usr/local/bin/

USER 10001:10001

EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

CMD ["api"]
