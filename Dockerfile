FROM golang:1.27-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/all2api ./cmd/all2api
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/doubao-browser-worker ./cmd/doubao-browser-worker
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/media-worker ./cmd/media-worker

FROM alpine:3.22

RUN apk add --no-cache ca-certificates wget chromium \
    && addgroup -S all2api && adduser -S -G all2api all2api \
    && mkdir -p /app/data \
    && chown -R all2api:all2api /app
COPY --from=build /out/all2api /usr/local/bin/all2api
COPY --from=build /out/doubao-browser-worker /usr/local/bin/doubao-browser-worker
COPY --from=build /out/media-worker /usr/local/bin/media-worker
USER all2api
WORKDIR /app
EXPOSE 8888
ENTRYPOINT ["/usr/local/bin/all2api"]
