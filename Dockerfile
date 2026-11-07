# Multi-stage build: a static Go binary on a small Alpine base with BlueZ/D-Bus
# for Bluetooth. Builds natively or cross-builds (deploy.ps1 / deploy.sh build
# linux/arm64 for the Raspberry Pi).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tesla-charge-control ./server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata bluez dbus
COPY --from=build /tesla-charge-control /usr/local/bin/tesla-charge-control
COPY config.example.yaml /config.example.yaml
EXPOSE 8080
ENTRYPOINT ["tesla-charge-control"]
# Mount your config at /config.yaml, or run with "-demo" to try it without hardware.
CMD ["-config", "/config.yaml"]
