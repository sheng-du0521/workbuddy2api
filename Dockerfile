# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
# 可选：Go 模块代理。官方 proxy.golang.org 在中国大陆不可达（go mod download
# 会卡死）；国内机器构建时传 --build-arg GOPROXY=https://goproxy.cn,direct。
# 留空 = 官方默认，行为与上游原版一致。
ARG GOPROXY=""
RUN test -z "${GOPROXY}" || go env -w GOPROXY="${GOPROXY}"
COPY go.mod ./
RUN go mod download
COPY . .
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
#
# v0.31.5：旧网关（cmd/server）已随合并删除，镜像里只有 webapp + 各 CLI 工具。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/webapp ./cmd/webapp \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trial_bin ./cmd/trial \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/activity_bin ./cmd/activity

FROM alpine:3.20
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/webapp /app/webapp
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY --from=build /out/trial_bin /app/trial_bin
COPY --from=build /out/activity_bin /app/activity_bin
COPY login.sh signin.sh credit.sh trial.sh /app/
# 国际版注册地区自动完善模块（login.sh global 分支 import；scripts/ 无测试/缓存）
COPY scripts/global_region.py /app/scripts/global_region.py
COPY scripts/task_common.py /app/scripts/task_common.py
COPY scripts/task_runner.py /app/scripts/task_runner.py
COPY scripts/school_open_day_2026.py /app/scripts/school_open_day_2026.py
# 成长任务入口（v0.31.59，审查 F05）：Go 调度器直接 exec 这个文件，
# 漏了它 → 标准镜像里的成长任务必然"脚本不存在"。依赖 task_runner / task_common（已在上）。
COPY scripts/growth_run.py /app/scripts/growth_run.py
RUN sed -i 's/\r$//' /app/*.sh && chmod 755 /app/*.sh
RUN sed -i 's/\r$//' /app/scripts/*.py && chmod 755 /app/scripts/*.py
USER app
# webapp 的唯一外部端口（网站 / 管理后台 / OpenAI 兼容网关共用）。
EXPOSE 8780
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8780/healthz || exit 1
# 入口 = webapp（v0.31.4 起；旧网关 wb2api 仍在镜像里但没有容器入口）。
#   · -db 落在 data 卷（SQLite + WAL，见 README 的备份说明）；
#   · 不放管理密钥：走 data/admin.key（首次启动随机生成并落盘）或 WB2API_ADMINKEY。
ENTRYPOINT ["/app/webapp", "-addr", "0.0.0.0:8780", "-db", "/app/data/webapp.db"]
