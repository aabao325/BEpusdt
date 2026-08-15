# 前端产物与目标架构无关，固定在构建机架构上跑，避免多架构构建时被 QEMU 模拟拖慢
FROM --platform=$BUILDPLATFORM node:25.2.1 AS web_builder

# 安装 pnpm
RUN npm install -g pnpm@10

WORKDIR /web
COPY web/package.json web/pnpm-lock.yaml ./

RUN pnpm install --frozen-lockfile --shamefully-hoist

COPY web/ ./
RUN pnpm run build:prod

# 同样固定在构建机架构，靠 CGO_ENABLED=0 交叉编译出目标架构二进制
FROM --platform=$BUILDPLATFORM golang:1.26.2-alpine3.23 AS builder

ENV GO111MODULE=on
WORKDIR /go/release
ADD . .

COPY --from=web_builder /web/dist ./static/secure

ARG VERSION=unknown
# buildx 自动注入，单架构构建时为空则回落到构建机默认值
ARG TARGETOS
ARG TARGETARCH

RUN set -x \
    && MODULE_PATH=$(go list -m) \
    && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath \
    -ldflags="-X '${MODULE_PATH}/app.Version=${VERSION}' -s -w -buildid=" \
    -o bepusdt ./main

FROM alpine:3.20

ENV TZ=Asia/Shanghai

# 安装所需的依赖
RUN apk add --no-cache tzdata ca-certificates

COPY --from=builder /go/release/bepusdt /usr/local/bin/bepusdt

# 设置时区
RUN ln -fs /usr/share/zoneinfo/Asia/Shanghai /etc/localtime

EXPOSE 8080
ENTRYPOINT ["bepusdt"]
CMD ["start"]
