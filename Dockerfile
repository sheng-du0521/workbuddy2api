# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
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
# 说明（v0.31.4 合并第一步）：`cmd/server`（旧网关）仍会编译，但**不再是本镜像的入口**
# —— 入口改为 webapp（网站 + 管理后台 + 网关一体）。旧网关按计划在合并第二步删除，
# 届时这一行与 /app/wb2api 一起去掉。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/webapp ./cmd/webapp \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server \
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
COPY --from=build /out/wb2api /app/wb2api
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
RUN sed -i 's/\r$//' /app/*.sh && chmod 755 /app/*.sh
RUN sed -i 's/\r$//' /app/scripts/*.py && chmod 755 /app/scripts/*.py
# 镜像不带真实配置：落 example 作为旧网关的默认（webapp 不用它，见下）。
COPY config.example.json /app/config.json
USER app
# webapp 的唯一外部端口（网站 / 管理后台 / OpenAI 兼容网关共用）。
EXPOSE 8780
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8780/healthz || exit 1
# 入口 = webapp（v0.31.4 起；旧网关 wb2api 仍在镜像里但没有容器入口）。
#   · -db 落在 data 卷（SQLite + WAL，见 README 的备份说明）；
#   · 不放管理密钥：走 data/admin.key（首次启动随机生成并落盘）或 WB2API_ADMINKEY。
ENTRYPOINT ["/app/webapp", "-addr", "0.0.0.0:8780", "-db", "/app/data/webapp.db"]
