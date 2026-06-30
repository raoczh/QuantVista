# ============================================================
# QuantVista 单镜像多阶段构建（参照 new-api）：
#   1) webbuilder  : 构建 Vue 前端，产物落到 server/web/dist
#   2) gobuilder   : go:embed 前端产物 → 编译单二进制
#   3) runtime     : slim 运行镜像，仅含二进制 + 证书/时区/wget
# 部署为单容器，后端托管前端，无需单独的 web 容器。
# ============================================================

# ---- 阶段 1：前端构建 ----
FROM node:20-alpine AS webbuilder
WORKDIR /app/web
COPY web/package.json ./
RUN npm install
COPY web/ ./
# vite.config 的 outDir 为 ../server/web/dist，故产物落在 /app/server/web/dist
RUN npm run build

# ---- 阶段 2：后端编译（embed 前端产物）----
FROM golang:1.25-alpine AS gobuilder
ENV GO111MODULE=on CGO_ENABLED=0 GOOS=linux GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
COPY VERSION ./VERSION
# 用真实前端构建产物覆盖占位 index.html，供 go:embed 打包
COPY --from=webbuilder /app/server/web/dist ./web/dist
RUN go build -ldflags "-s -w -X 'quantvista/common.Version=$(cat ./VERSION)'" -o /quantvista .

# ---- 阶段 3：运行镜像 ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget && update-ca-certificates
ENV TZ=Asia/Shanghai
COPY --from=gobuilder /quantvista /quantvista
WORKDIR /data
EXPOSE 3000
ENTRYPOINT ["/quantvista"]
