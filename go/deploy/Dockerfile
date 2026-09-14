# syntax=docker/dockerfile:1

# cchd（Go 数据面 + 管理面 + 嵌入 UI）生产镜像。
#
# 环境变量与部署契约：DSN / REDIS_URL / ADMIN_TOKEN / PORT / CCH_EGRESS_PAGES 等，
# 见仓库根 .env.example；换镜像不需要改部署文件。
#
# 构建（仓库根目录，单架构）：
#   docker build -t cchd:local -f go/deploy/Dockerfile .
#
# 构建（多架构，需 buildx）：
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     --build-arg APP_VERSION=1.6.0 -f go/deploy/Dockerfile .
#
# 受限网络（离线/镜像仓库不可达）：用 go/deploy/Dockerfile.prebuilt —— 先把二进制在
# 构建机 `go build` 出来，再打包成镜像；该路径不需要任何容器内网络（含 apk）。
#
# 前置：go/internal/uiapp/assets 必须是**已收编的产物**（压缩态更省体积），
# 生成顺序：bun scripts/build-ui-export.mjs && bun scripts/build-ui-embed.mjs。
# 该目录被 .gitignore 忽略，CI 必须先跑上述两步再 docker build。

# BUILDPLATFORM 是 BuildKit/buildx 的自动参数（= 构建机平台），此处给出默认值，
# 好让**没有 buildx 的传统 builder** 也能构建（它不会定义该自动参数，缺默认值会报
# `failed to parse platform : ""`）。buildx 会用真实构建机平台覆盖它。
ARG BUILDPLATFORM=linux/amd64

ARG GO_IMAGE=docker.m.daocloud.io/library/golang:1.25-alpine
ARG RUNTIME_IMAGE=docker.m.daocloud.io/library/alpine:3.20

# ---------------------------------------------------------------------------
# 构建阶段：编译静态二进制。
# --platform=$BUILDPLATFORM -> builder 跑在**构建机原生架构**上，靠 GOARCH 交叉编译到目标架构；
#   否则 buildx 多架构构建会让 builder 走 QEMU 模拟（慢一个数量级且更易失败）。
# CGO_ENABLED=0 -> 纯静态（pgx / go-redis 皆纯 Go），运行阶段无需 libc。
# -trimpath       -> 去掉构建机绝对路径（可复现且不泄漏路径）。
# -s -w           -> 去符号表与 DWARF，实测省 11 MiB（44.7 -> 33.9 MiB）。
# ---------------------------------------------------------------------------
FROM --platform=${BUILDPLATFORM} ${GO_IMAGE} AS builder

WORKDIR /src

# TARGETARCH 由 buildx 按目标平台注入；传统 builder 不定义它，故构建命令用 shell 默认值兜底。
ARG TARGETARCH

# GOPROXY/GOSUMDB 可覆盖：受限网络用 `--build-arg GOPROXY=https://goproxy.cn` 等。
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=${GOPROXY} GOSUMDB=${GOSUMDB}

# 先只拷依赖清单，令 go mod download 层在源码变动时依旧命中缓存。
COPY go/go.mod go/go.sum ./
RUN go mod download

COPY go/ ./
# UI 产物在 go/internal/uiapp/assets，随上面的 COPY 一起进入（.gitignore 忽略，故 CI 必须先生成）。
# 缺产物时**构建仍会成功**（embed 只要求路径存在，`.gitkeep` 即可匹配），但以
# CCH_EGRESS_PAGES=embed 启动时 uiapp 会 fail fast 拒给启动（不静默降级为空页面）；
# 故部署后用 `/readyz` 的 `notes.pages` 确认 embed 文件数（2026-09 实测 502 个 / 5.1 MiB；
# 数字随导出页数与语种数变化，报 0 或缺失就是没生成）。

RUN CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH:-amd64}" go build \
        -trimpath \
        -ldflags='-s -w' \
        -o /out/cchd \
        ./cmd/cchd

# ---------------------------------------------------------------------------
# 运行阶段：alpine + CA 根证书（**从 builder 拷，不跑 apk**）。
# 选 alpine 而非 distroless/scratch 的理由（实测）：
#   1) 需要 CA 根证书（上游 TLS 与 PG TLS）；
#   2) HEALTHCHECK 需要一个能发 HTTP 的最小客户端，busybox wget 足够；
#   3) adduser 用于建非 root 账号，/sbin/nologin 提供无 shell 的账号语义。
# 时区表已由 Go 的 time/tzdata 内嵌，无需 tzdata 包；UI 走 go:embed，无需写磁盘。
#
# CA **从 builder 阶段拷贝**而非 `apk add --no-cache ca-certificates`：
# 后者让运行阶段依赖构建期网络（实测在无 DNS/无 socket 的构建环境会直接失败），
# 且多出一层包管理器缓存层。golang 基础镜像自带同一份 Mozilla 根证书束。
# ---------------------------------------------------------------------------
FROM ${RUNTIME_IMAGE} AS runtime

ARG APP_VERSION=dev
# 版本号进进程环境：`internal/appversion` 读 APP_VERSION，于是 /api/version、/api/health、UI 壳
# 三处报同一个值。发布脚本用同一版本号做镜像 tag（见 release 脚本），故 tag 与进程自报值
# 不可能再分叉。为让 `docker inspect` 也能看到，同一个参数同时进 LABEL。
ENV APP_VERSION=${APP_VERSION}
LABEL org.opencontainers.image.title="claude-code-hub-go cchd" \
      org.opencontainers.image.version="${APP_VERSION}" \
      org.opencontainers.image.description="Go 数据面/管理面后端，内嵌 UI 静态产物"

RUN adduser -D -H -s /sbin/nologin cchd

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/cchd /usr/local/bin/cchd

# 默认值与 Go 侧 config 的 PORT 默认值一致（23000）；部署改 PORT 时探针自动跟随，
# 否则自定 PORT 的部署会因探针打错端口而永远 unhealthy。
ENV PORT=23000

# 页面归属：代码默认值是 `off`（Node 双跑过渡期的保守取值——缺省不把页面交给
# 可能已退役的后端）。但本镜像里 UI 已 embed 进二进制，**没有别的进程能答页面**：
# 不设此项，用户 `docker run` 后打开站点会看不到界面。故镜像显式设为 `embed`，
# 与生产部署一致。想关掉页面面（例如前面有独立托管的静态站）可显式覆盖为 `off`。
ENV CCH_EGRESS_PAGES=embed
USER cchd
# 公共端口由本进程独占（已无「Node 退到内部端口双跑」这回事）。
EXPOSE 23000
# Go 侧实现了排空关闭：SIGTERM -> 停止接纳 -> 等在途请求与待结算，故必须让 SIGTERM 生效。
STOPSIGNAL SIGTERM
# 探针用 /readyz（校验 PG、Redis、规则快照，排空中会返回 503），不用 /v1/_ping：
# 后者只证明进程活着，不证明依赖可用。用 shell 形式以便展开 ${PORT}。
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${PORT:-23000}/readyz" || exit 1

ENTRYPOINT ["/usr/local/bin/cchd"]
