# ---- 构建阶段 ----
FROM golang:1.23-alpine AS build

WORKDIR /src

# 先拷依赖清单，命中缓存层
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO_ENABLED=0：modernc.org/sqlite 是纯 Go 实现，可以完全静态编译
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/t2c ./cmd/t2c

# ---- 运行阶段 ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata wget \
 && addgroup -g 10001 -S t2c \
 && adduser -u 10001 -S t2c -G t2c

COPY --from=build /out/t2c /usr/local/bin/t2c

# 数据目录归非 root 用户所有
RUN mkdir -p /data && chown t2c:t2c /data

USER t2c
ENV T2C_ADDR=:8080 \
    T2C_DATA_DIR=/data

EXPOSE 8080
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz > /dev/null || exit 1

ENTRYPOINT ["t2c"]
