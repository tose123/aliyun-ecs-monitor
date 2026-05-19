FROM golang:1.24.0-bookworm AS builder

WORKDIR /src

ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY config.example.yaml ./config.example.yaml

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/aliyun-ecs-monitor ./cmd/aliyun-ecs-monitor

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/leechao/aliyun-ecs-monitor"

COPY --from=builder /out/aliyun-ecs-monitor /aliyun-ecs-monitor

ENTRYPOINT ["/aliyun-ecs-monitor", "-config", "/config.yaml"]
