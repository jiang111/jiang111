FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.commit=$(cat VERSION 2>/dev/null)" -o /emby-proxy .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /emby-proxy /usr/local/bin/emby-proxy
ENV CONFIG=/data/config.json TZ=Asia/Taipei
VOLUME /data
ENTRYPOINT ["emby-proxy"]
