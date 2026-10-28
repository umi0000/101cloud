# 101cloud 宿主机 Agent

Go 节点服务。业务后端只连接 Agent，Agent 通过本机 Unix socket 控制 Incus。用户、规格、pt 账本和跨节点调度留在业务后端。

本目录是独立 Go 模块。以下命令均在 `host-agent/` 目录执行。

初始化、后端接入和各类 API 的完整示例见 [Agent 使用指南](docs/AGENT使用指南.md)。

## 快速开始

```bash
go build -o bin/hostagent ./cmd/hostagent

# endpoint 必须是后端实际访问的地址，带显式端口。
# 目录必须不存在；初始化不连接 Incus、不安装驱动、不修改宿主机网络。
bin/hostagent init --state-dir ./agent-state --endpoint https://127.0.0.1:8443

bin/hostagent -config ./agent-state/agent.json -check
bin/hostagent -config ./agent-state/agent.json
```

初始化自动生成：

| 文件 | 用途 |
| --- | --- |
| `agent.json` | 节点服务配置 |
| `server.crt` / `server.key` | 自签 HTTPS 证书与私钥 |
| `token` | 本节点随机认证密钥 |
| `connection.json` | 供业务后端导入的连接配置，包含地址、Token 和证书指纹 |

**把 `connection.json` 通过可信渠道导入后端即可，不需要客户端证书或 CA。** 它是管理员凭据，不应发给普通用户、提交源码、放进 URL 或记录日志。目录权限为 0700，敏感文件为 0600。初始化只打印文件位置，不打印 Token；重复初始化不会覆盖已有目录。

生产示例（仅供节点管理员执行，本项目开发不会自动执行）：

```bash
hostagent init --state-dir /etc/101cloud-agent --endpoint https://192.168.1.99:8443 --listen 192.168.1.99:8443
hostagent -config /etc/101cloud-agent/agent.json -check
```

默认监听地址由 endpoint 推导。endpoint 是节点可达地址，不能填写 `0.0.0.0`、`::` 等监听通配地址；需要通配监听时用独立的 `--listen`。证书 SAN 根据 endpoint 的 IP 或 DNS 自动生成。默认有效期为一年，可用 `--validity 8760h` 指定；不会在运行中静默更换证书。Incus socket 默认 `/var/lib/incus/unix.socket`，可用 `--incus-socket` 指定。

## 后端导入

连接文件格式：

```json
{
  "version": 1,
  "endpoint": "https://192.168.1.99:8443",
  "token": "<自动生成的节点密钥>",
  "server_cert_sha256": "<自签叶证书DER的SHA256，64位小写十六进制>"
}
```

以上展示格式，示例占位值不是有效凭据。后端 Go 接入：

```go
connection, err := client.LoadConnection("connection.json")
if err != nil {
    return err
}
httpClient, err := client.NewHTTPClient(connection)
if err != nil {
    return err
}
defer httpClient.CloseIdleConnections()
request, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.Endpoint+"/agent/v1/health", nil)
if err != nil {
    return err
}
response, err := httpClient.Do(request)
if err != nil {
    return err
}
defer response.Body.Close()
```

导入 `101cloud/hostagent/client`，请求由客户端自动添加 `Authorization: Bearer ...`。客户端只允许访问该配置的 HTTPS origin，不跟随重定向、不使用环境 HTTP 代理，也没有会中断事件/备份流的全局短超时；调用方通过 context 设置操作期限。

连接配置还可由后端自己的节点录入接口解析为 `client.ConnectionConfig`，交给 `NewHTTPClient` 校验，不要求后端一定使用文件存储。当前项目提供的是 Agent 与 Go 客户端，不包含业务管理网页。

## 安全通信

- TLS 1.3 加密；后端逐次核对叶证书 SHA256 指纹、有效期、DNS/IP 和服务端用途，不接受指纹变化，不自动信任首次连接。
- 自签证书没有公共 CA 信任链，客户端使用显式导入的证书指纹作为信任锚，不是关闭验证后接受任意证书。
- 每个节点使用独立的 256 bit 随机 Token，以 base64url 无 padding 编码。Agent 恒定时间比较认证摘要；所有原生 API、扩展 API和升级通道都必须认证。
- Token 只通过 Authorization 请求头发送，不接受查询参数或 Cookie 认证，认证后不转发 Token 给 Incus。
- 审计不记录请求体、Authorization 和 URL 查询参数；Incus WebSocket 查询中的 secret 同样不会写入日志。
- 完整 Incus 管理权等同于宿主机管理员权限。只向业务后端开放管理端口，仍建议使用管理网/VPN 和来源防火墙。

证书或 Token 泄露时，先停止 Agent，再使用新状态目录重新初始化、更新服务配置并重新导入后端连接信息。不要把重新初始化当作自动信任换证。配置在启动时读取，重启服务会关闭旧连接与 WebSocket，但不停止用户实例。证书到期前须安排显式换证和重新导入；不提供隐式自动换证或旧凭据兼容窗口。

旧的 mTLS CA、客户端证书白名单、`agent-pki` 命令及旧客户端构造接口已移除，旧配置不会自动降级或继续兼容。

## 管理接口

| 路径 | 能力 |
| --- | --- |
| `/`、`/1.0`、`/1.0/*` | 完整 Incus 原生 API，含实例、项目、网络、存储、镜像、备份、操作、事件、exec/控制台 WebSocket 和 SFTP Upgrade |
| `GET /agent/v1/health` | 实时检查 Incus 连通性，不允许匿名访问 |
| `GET /agent/v1/resources` | 完整 `server_resources`、所有 GPU 型号与指标 `gpus`、节点综合平均负载 `average_load_percent` |
| `PUT /agent/v1/instances/{name}/gpu?project=default` | 使用 UUID 配置 GPU，PCI 解析留在本机 |

GPU 指标由宿主机 `nvidia-smi` 采集。每卡综合负载为 GPU 利用率与显存占用率的等权平均，节点负载为所有卡分数的等权平均；采样失败用 null 和错误说明表示，不作为空闲。它不是历史时间平均，也不保证仍有足够显存。字段和计算示例见使用指南第 5.2 节。

原生 API 的版本与扩展以节点 Incus 为准。请求超时不等于操作失败，后端应保存 operation ID 并核对状态，不能盲目重复创建或释放资源。跨节点迁移仍需后端编排连接地址、认证和失败恢复。

GPU 请求：

```json
{
  "gpu_uuids": ["GPU-7f665bf9-5911-4f71-ac6a-a66e32362574"],
  "allow_live": false
}
```

- 多 UUID 为多卡，空数组移除 Agent 管理的显卡配置；只管理 `agent-gpu-N`，不静默删除其他 GPU 或 profile 配置。
- 默认要求实例已停止。`allow_live: true` 显式允许在线改入口，**不迁移 CUDA context、不杀旧进程、不保证旧卡显存释放**。
- CPU 工作空间以后要在线接卡，应在首次启动前准备 `nvidia.runtime=true` 和库环境；热插卡不重新执行 NVIDIA 启动 hook。
- 使用 ETag 防止覆盖配置；返回 `operation_id`、`operation_completed`、`configuration_state`、`actual`、`etag`，失败不假定已回滚。
- `resource_release` 始终是 `not_verified`，配置成功不等于设备挂载和进程状态一致。配额、共享人数、计费和实际占用核验仍由业务层组织。

## 构建与部署

需要 Go 1.24 或更新版本，Incus SDK 锁定 v6.0.5。

```bash
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/hostagent-linux-amd64 ./cmd/hostagent
```

`deploy/agent.example.json` 展示生成配置的结构，相对证书/Token 路径相对于配置文件目录。生产推荐直接使用初始化生成的配置，避免手写密钥和证书路径。

`deploy/101cloud-agent.service` 使用 `/usr/local/bin/hostagent` 和 `/etc/101cloud-agent/agent.json`。默认 root 运行并启用系统限制；专用用户加 Incus socket 管理组也属于高权限，不能视为宿主机隔离边界。

本地测试与协议烟测不代替真实节点验收。未经明确授权，不 SSH，不在服务器安装或启动 Agent；真实 Incus 实例、备份恢复和 NVIDIA 计算需在获得授权后验收。

## Ubuntu 宿主机 AppArmor 兼容性与持久化

### 适用情况

Ubuntu 25.04/25.10/26.04 宿主机上，Incus 非特权容器可能出现真实 SSH/PTY 内 `apt update` 挂起，但无终端的 Agent exec 可以成功的情况。已实测 Ubuntu 26.04 宿主机、Incus 6.0.5、Ubuntu 24.04 容器：APT 的 Ubuntu Pro 前置钩子启动 `systemctl`，其终止 `systemd-tty-ask-password-agent` 的信号被 AppArmor 拒绝，导致更新在下载前等待。

不能只凭 apt 慢就套用此方案。应先检查进程链与宿主机内核日志，确认 `apparmor="DENIED"`、`operation="signal"`、`comm="systemctl"` 和相关容器标签；网络、软件源、锁等待等其他问题需分别排查。Agent 不会自动修改下述参数。

Ubuntu 24.04 **宿主机**通常默认不启用这一项额外限制，因此可作为候选基线，但不保证所有更新、加固配置或内核/Incus组合均无此问题。仅把容器系统换为 Ubuntu 24.04 不足以规避宿主机限制。我们尚未在 Ubuntu 24.04 宿主机上完成同等验收。

### 检查与持久化

以下命令在 **Ubuntu 宿主机**执行，不是在容器中执行；先记录原值：

```bash
sysctl kernel.apparmor_restrict_unprivileged_unconfined
sysctl kernel.apparmor_restrict_unprivileged_userns
```

确认接受下面的安全取舍后，创建独立配置并只加载此文件：

```bash
sudo tee /etc/sysctl.d/99-incus-apparmor-compat.conf >/dev/null <<'EOF'
# Compatibility for Incus with Ubuntu's extra AppArmor restrictions.
kernel.apparmor_restrict_unprivileged_unconfined = 0
EOF

sudo sysctl -p /etc/sysctl.d/99-incus-apparmor-compat.conf
sysctl kernel.apparmor_restrict_unprivileged_unconfined
```

预期输出 `kernel.apparmor_restrict_unprivileged_unconfined = 0`。系统启动时会加载该配置；如其他 sysctl 配置也设置了同一项，需要检查加载顺序，避免后加载的文件覆盖回 1。不要顺带将 `kernel.apparmor_restrict_unprivileged_userns` 设为 0；本次验证中它始终保持 1。

### 现有容器与验收

仅修改运行时参数、重新 SSH，不一定刷新既有容器的安全上下文。本次在参数为 0 时，**通过 Host Agent 仅重启受影响容器**后，真实 SSH 下普通 `apt update` 才恢复，2.88 秒退出 0；systemd 与 CUDA 复验通过。

重启使用 Agent 代理的原生接口 `PUT /1.0/instances/{name}/state?project=default`，请求体为 `{"action":"restart","timeout":30,"force":false,"stateful":false}`。应等待异步操作成功及 systemd 就绪，再用真实 SSH/PTY 验证；不能仅用非交互 exec 判定修复。重启会中断该容器的会话和任务，须先确认可停机，不要批量重启其他实例。仅重启 Agent 服务不会替代容器重启。

如果运行时参数已经是 0、受影响容器也已重启验证，写入持久配置本身不需要再次重启容器或宿主机。

### 安全影响与回退

设为 0 **不是关闭整个 AppArmor**，也不取消非特权容器、UID/GID映射、seccomp、capabilities或资源配额。但它会在整台宿主机范围取消一层针对低权限 unconfined 进程切换 AppArmor profile 的额外防绕过限制，不只影响某一个容器。仍保持 userns 开关为 1，不等于这层防绕过能力没有减弱。多人可执行任意代码的平台应明确评估该取舍，不能把它描述成无安全影响的配置。

长期应验证兼容的内核、AppArmor和Incus版本组合，不能未经测试承诺升级一定修复。原理和上游讨论见 [Ubuntu 安全团队说明](https://discourse.ubuntu.com/t/understanding-apparmor-user-namespace-restriction/58007)、[Ubuntu 缺陷报告](https://bugs.launchpad.net/bugs/2109394)。

若该文件确实由上述步骤新建、且没有加入其他自定义项，可以这样撤销；运行时值应恢复为你事先记录的原值，下面以原值 1 为例：

```bash
sudo rm /etc/sysctl.d/99-incus-apparmor-compat.conf
sudo sysctl -w kernel.apparmor_restrict_unprivileged_unconfined=1
```

回退可能重新触发兼容性问题，也不能把已有进程的上下文恢复视为即时完成；需安排受影响容器的重启与复验。不要删除其他系统或管理员提供的 sysctl 文件。本节只是人工操作指南，Agent 初始化和启动不会自动执行这些变更。

## 本地验证记录

已通过全包竞态测试、`go vet`、构建与 Linux amd64 交叉构建。实际执行了初始化、重复初始化拒绝和配置预检；运行真实 Agent 与 Go 后端客户端，通过导出的连接文件访问 health、原生实例 API 和 WebSocket。错误 Token 返回 401，错误指纹在发送 HTTP 请求前被拒绝，Token 未转发到 Unix socket 上游。Incus 上游使用本地协议测试服务，不代表本次已在真实 Incus/GPU 节点部署或验收。
