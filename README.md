# dockerCopilot
<a href="https://www.gnu.org/licenses/agpl-3.0.en.html">
    <img alt="License: AGPLv3" src="https://shields.io/badge/License-AGPL%20v3-blue.svg">
  </a>

> **关于本仓库（修改版声明）**
>
> 本仓库是 **[onlyLTY/dockerCopilot](https://github.com/onlyLTY/dockerCopilot) 的修改版**，
> 基于上游 v2.1.3（上游提交 `2f975ce`）二次开发。
> 维护者：[@aaron2024S](https://github.com/aaron2024S)；**最近修改日期：2026-09-25**。
>
> 相对上游的主要改动：
> 1. 后端新增容器自动更新（`internal/**/autoupdate`、`internal/module/autoupdateplan.go`）与端口映射采集（`internal/utiles/portinfo.go`）
> 2. 前端内置到本仓库 `frontend/`（源自 [dongshull](https://github.com/dongshull) 的 Docker Copilot 前端，其 README 标注 MIT 许可），CI 不再从第三方仓库拉取前端
> 3. 新增离线镜像构建脚本 `offline/`、`.gitattributes`（强制 LF，避免容器内 `start.sh` 报 `bad interpreter`）
> 4. 修正 CI 中前端产物的上传路径（本项目的 Vite `outDir` 是 `front`，不是 `dist`）
>
> 上游版权归原作者所有；本仓库整体仍以 **AGPL-3.0**（见 [LICENSE](LICENSE)）发布。

# 介绍

一个主打便捷的docker容器管理工具，现在已经支持所有平台。
已经实现：
1. 一键更新容器
2. 指定镜像和tag更新
3. 启动、停止、重启容器
4. 重命名容器
5. 删除无TAG镜像
6. 删除未使用镜像
7. 更新进度查看
8. 备份容器设置
9. 恢复容器设置
10. 容器自动更新（定时重建有新版本的容器，可选删除旧镜像，支持开关与排除列表）
11. 容器卡片显示端口映射（已映射到宿主机的端口高亮，超过 2 个收起成「+N」，点开详情看全部；
    host 网络容器没有映射记录，改用镜像 `EXPOSE` 列出它占用的端口）

## 使用

docker compose 安装

```
services:
  dockercopilot:
    container_name: dockercopilot
    restart: always
    privileged: true
    network_mode: bridge
    ports:
      - 12712:12712
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./data:/data
    environment:
      - TZ=Asia/Shanghai
      - DOCKER_HOST=unix:///var/run/docker.sock
      - secretKey=密码，不少于八位且非纯数字
    image: 0nlylty/dockercopilot:latest
```

## 开发环境

go版本：1.21+

## 目录结构

```
├── internal/     # Go 后端
├── frontend/     # React 前端源码（Vite + Tailwind）
├── front/        # 前端构建产物，通过 //go:embed front/* 打进二进制
└── offline/      # 离线镜像构建（无需 Docker 引擎，可产出可分发的镜像 tar）
```

## 构建

前端构建产物直接输出到 `front/`，构建完直接 `go build` 即可，无需手工拷贝：

```bash
cd frontend
npm install
npm run build    # 产物写入 ../front/

cd ..
go build         # 打包进单个二进制
```

容器自动更新功能的接口契约、配置字段与执行时序见 [docs/auto-update.md](docs/auto-update.md)。

## 离线镜像构建（没有 Docker 也能出镜像）

`offline/` 下有一套纯 Python 工具链，**不需要 Docker 引擎**：
走 registry v2 HTTP API 拉基础镜像层、手工拼装 `docker save` 格式的归档，
产出可直接 `docker load -i` 导入的镜像 tar（默认只出 amd64，需要 ARM 才加 `-Arch arm64`）。

```powershell
cd offline
.\build.ps1                    # 组镜像 + 静态校验 + 启动行为校验
```

产物、部署步骤、构建原理与排错见 [offline/OFFLINE-BUILD.md](offline/OFFLINE-BUILD.md)。

