# Vendored MSM 来源、许可与本地改动

本目录是 Go agent 安装器嵌入的 CS2 多实例管理资源。

- 上游维护仓库：[GamesTwoLife/cs2-multiserver](https://github.com/GamesTwoLife/cs2-multiserver)；原项目：[dasisdormax/cs2-multiserver](https://github.com/dasisdormax/cs2-multiserver)。
- 许可：Apache License 2.0，见同目录 `LICENSE`。保留上游许可与本说明。
- 本地补丁：`patches/arena-local.patch`，针对 GOTV 端口分配、实例级 clone、启动参数和依赖版本配置。此快照包含本地修改，未获上游背书。
- `cs2-server` 符号链接在安装时生成；嵌入资源中的 `msm` 可执行权限在安装时恢复。
