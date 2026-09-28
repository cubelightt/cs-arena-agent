# CS Arena Agent v2.0

CS Arena Agent 是 Counter-Strike 2 游戏主机使用的 Go 桥和命令行工具。桥通过 reverse WebSocket 主动连接 [CS Arena 平台](https://github.com/cubelightt/cs-arena)，接收实例与比赛操作；[ArenaMatch](https://github.com/cubelightt/cs-arena-match) 插件通过本机 IPC 与桥通信。

## 下载或构建

Linux amd64 编译版本及 SHA-256 校验文件见 [v2.0 Release](https://github.com/cubelightt/cs-arena-agent/releases/tag/v2.0)。下载后将二进制安装为 `cs`：

```bash
mkdir -p "$HOME/data/bridge"
install -m 0755 cs-agent-linux-amd64 "$HOME/data/bridge/cs"
```

也可使用 `go.mod` 声明的 Go 版本或更新版本从源码构建：

```bash
./scripts/build.sh
./cs version
```

默认版本为 `v2.0`，输出为 `./cs`；可用 `VERSION` 指定版本、`OUT` 指定输出路径。构建使用 `internal/install/` 下的内嵌安装资源。

## 主机配置

桥运行在 Linux 游戏主机，使用 MSM 管理 CS2 实例。准备非 root 运行账号、MSM、游戏文件及相应主机依赖，将 [`config.example.yaml`](config.example.yaml) 复制为 `$HOME/data/bridge/config.yaml`，填写配置并设置权限为 `0600`。

| 配置项 | 说明 |
|---|---|
| `token` | 与平台服务器组一致的桥令牌，必填 |
| `backend_ws` | 平台 WebSocket 地址，如 `wss://arena.example.com/api/agent`，必填 |
| `msm_dir` | MSM 根目录，必填 |
| `msm` | MSM 命令路径，可选，默认由根目录推导 |
| `demo_dir` | 主机 Demo 目录，可选 |
| `archive_dir` | 主机归档目录，可选 |

实例清单与平台状态由连接后的平台同步。桥不对外监听 HTTP 端口。先检查主机条件，再启动桥：

```bash
~/data/bridge/cs --config ~/data/bridge/config.yaml doctor
~/data/bridge/cs agent --config ~/data/bridge/config.yaml
```

`cs install --check` 可检查安装条件；`cs install` 提供 MSM、桥配置和 systemd 用户服务的安装入口，参数见 `cs help`。ArenaMatch 部署方法见[插件 README](https://github.com/cubelightt/cs-arena-match)。

## systemd 服务

[`deploy/cs-agent.service`](deploy/cs-agent.service) 默认使用 `$HOME/data/bridge/cs` 和同目录 `config.yaml`。修改路径后安装到用户服务目录：

```bash
mkdir -p ~/.config/systemd/user
cp deploy/cs-agent.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now cs-agent
systemctl --user status cs-agent --no-pager
journalctl --user -u cs-agent -f
```

需要用户登出后常驻时，可按主机权限设置 `loginctl enable-linger "$USER"`。平台 `/api/health` 中的桥状态应为 connected。

## 常用操作

以下示例假定 `cs` 已加入 PATH，配置位于默认路径 `$HOME/data/bridge/config.yaml`：

```bash
cs help
cs status
cs doctor
cs log <实例> --lines 100
cs start <实例>
cs stop <实例>
cs restart <实例>
cs update --check
cs jobs
```

用 `--config <路径>` 指定其他配置文件。实例、插件和文件操作的完整参数见 `cs help`；支持 `--dry-run` 的操作可先预览结果。

## 升级与资源

升级时停止桥服务，备份旧二进制和配置，替换 `cs` 后重新启动并确认连接。数据库、实例、游戏文件和归档应保留。

MSM 来源、许可及修改声明见 [`internal/install/msm/MODIFICATIONS.md`](internal/install/msm/MODIFICATIONS.md)。当前比赛插件为 ArenaMatch 0.6.15，使用 Go 桥协议 v2 和本机 IPC v1。

`cs install --arena-patches` 仅修复主机配置和共享链接，不安装或覆盖插件 DLL。

## 鸣谢

感谢以下开源项目及其贡献者：

| 项目 | 使用方式 |
|---|---|
| [CS2 Multi Server Manager](https://github.com/GamesTwoLife/cs2-multiserver) | 当前内嵌的多实例管理脚本来源，包含本项目的本地修改；该分支基于 [dasisdormax/cs2-multiserver](https://github.com/dasisdormax/cs2-multiserver) 原项目 |
| [Gorilla WebSocket](https://github.com/gorilla/websocket) | 当前桥与平台之间的 WebSocket 通信 |
| [go-yaml / yaml.v3](https://github.com/go-yaml/yaml/tree/v3.0.1) | 当前 YAML 配置解析 |

MSM 的许可证与修改说明见 [`internal/install/msm/LICENSE`](internal/install/msm/LICENSE) 和 [`MODIFICATIONS.md`](internal/install/msm/MODIFICATIONS.md)。第三方代码保留各自的许可证，本节鸣谢不替代相应的版权与许可声明。

## 开源协议

本项目自有代码采用 GNU General Public License v3.0 ，详见 [LICENSE](LICENSE)。
