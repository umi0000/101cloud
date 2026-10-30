# 101cloud Frontend

基于 [Vue Vben Admin](https://github.com/vbenjs/vue-vben-admin) v5.7.0（MIT，见 [LICENSE](LICENSE)）的 `web-antd` 应用裁剪而来，只保留 ant-design-vue 版本。设计与后端契约见 [前端设计.md](前端设计.md)。

当前版本只实现**管理员面板**。普通成员登录后看到"成员端暂未开放"提示页；后端对 `/api/v1/admin/*` 仍强制 admin 角色。

管理员操作不填写、提交或新增记录操作理由；基础审计仍保留操作者、动作、时间，历史记录不删除。确认弹窗使用无参回调，保留危险操作提示、删除名称确认及强制操作风险确认；CSRF、幂等键和 If-Match 不变。控制台选择适用类型后连接，无理由输入。自定义实例需求在平台外沟通，平台仅保留管理员在实例页直接下发；不提供成员申请、管理员审批、申请说明或审批意见界面，旧申请地址不保留兼容跳转。

控制台（终端、串口、VGA）没有固定会话时长上限，标题仅显示类型和实例名称，不显示到期倒计时。登录会话到期（12 小时）、退出登录、用户停用、权限撤销或实例停止仍会断开连接；每个用户最多同时打开 4 个控制台。断线后重新连接会创建新的单次使用会话，不复用旧会话。准备等待保持有界，网络心跳、帧/输入限制、终端尺寸调整及关闭时先 GET 取 ETag 再 DELETE 的流程不变。

## 目录

```text
apps/web-antd/src/
  api/request.ts     唯一请求客户端：CSRF、幂等键、If-Match、错误信封
  api/backend.ts     后端接口封装（路径与请求体对应 backend/openapi.json 与后端代码）
  api/types.ts       后端响应类型
  api/task-schema.ts 任务/步骤公开错误的具名边界校验
  composables/api.ts 游标分页、详情+ETag、一次意图一个幂等键的动作、任务跟踪、轮询
  components/        状态标签、时间、游标分页器、确认弹窗（名称/风险）、任务中心、ErrorFeedback
  locales/           错误码字典、枚举中文
  views/admin/*      管理端页面
```

`packages/`、`internal/` 为 Vben 上游代码，尽量不改，便于以后手动合并上游。

## 可读错误与任务失败

- 同步 API 和异步任务/步骤共用 `locales/errors.ts` + `ErrorFeedback`：优先显示后端安全、具体的中文原因，历史英文 message 用中文错误码表兜底，未类型化异常不回显原文。主标题短，资源数字、字段校验与中文处理建议分开展示；已写进原文的同一数量不会重复堆叠。
- `details` 显示配额/节点/常驻/存储池/IP/GPU 范围和资源维度，以及已用、本次申请、上限/物理总量、可用量。字节走 BigInt 的 `formatBytes`，计数保留十进制字符串，pt 只去尾零；错误码/阶段在技术详情，请求 ID 独立显示、可复制。
- 任务 API 通过 `TaskSchema` / `TaskStepSchema` 校验可选可空 `error: {code,message,details}`，兼容保留 `error_code` 的历史记录。任务中心通知、任务列表/详情、实例任务标签页与创建失败横幅展示具体原因及建议，而非只显示 `unknown` 或错误码。
- `unknown` 仍为结果待核对，绝不自动重放；`failed` 无盲目重试按钮。只有 `resource_reservations_released=true` 的实例显示已确认创建前失败及 IP/GPU/计费预留释放，并优先显示创建失败；false/缺失明确提示预留未确认释放。已就绪实例仍以真实 `state_stale` 显示状态未知，不伪造运行状态。
- 登录表单、控制台准备和指标查询保留完整错误说明与请求 ID；CSRF 原键重试、If-Match、幂等键和危险操作确认规则保持不变。历史任务与真实实例不会因前端展示更新而删除、重置或重试。


## 环境

- Node.js 22.18+ 或 24
- pnpm 10（`corepack enable pnpm`）

```bash
cd frontend
pnpm install
```

## 开发

浏览器只访问 Vite 开发服务器（`http://localhost:5555`），由它把 `/api/v1`（含 WebSocket）代理到开发后端，**同源，不做 CORS**。

1. 启动一个**开发专用**后端（独立数据库、测试账号），`CLOUD_ORIGIN` 必须与浏览器地址完全一致：

   ```dotenv
   CLOUD_ORIGIN='http://localhost:5555'
   CLOUD_LISTEN='127.0.0.1:18080'
   ```

   http 源下后端使用不带 Secure 的 `101cloud_session` Cookie，并在启动日志警告明文传输。只用于本机开发。

2. 指定代理目标（`.local` 文件不提交）：

   ```bash
   echo 'VITE_DEV_PROXY_TARGET=http://127.0.0.1:18080' > apps/web-antd/.env.development.local
   ```

   开发后端在另一台机器上时，用 SSH 隧道转发到本机回环端口，不要让开发后端监听局域网地址。

3. 启动：

   ```bash
   pnpm dev:antd
   ```

开发服务器只监听 `localhost`。必须用 `http://localhost:5555` 打开；用其他地址访问时 Origin 与 `CLOUD_ORIGIN` 不一致，所有写操作会被拒绝。

## 检查与构建

```bash
cd apps/web-antd
pnpm exec vue-tsc --noEmit --skipLibCheck   # 类型检查
pnpm build                                   # 产物在 apps/web-antd/dist
cd ../.. && pnpm exec oxlint apps/web-antd/src
```

## 生产部署

构建产物只请求同源相对路径 `/api/v1`，不含任何后端地址。由 Nginx 在同一 HTTPS 域名下提供静态文件并反代后端：

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    server_name cloud.example.com;           # 必须与后端 CLOUD_ORIGIN 完全一致
    # ssl_certificate / ssl_certificate_key ...

    root /var/www/101cloud;                  # apps/web-antd/dist 的内容
    index index.html;

    location /api/v1/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header Upgrade $http_upgrade;     # 终端 / VNC WebSocket
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 3600s;
    }

    location / {
        try_files $uri $uri/ /index.html;    # history 路由
    }

    add_header X-Frame-Options DENY always;
}
```

后端同时配置：

```dotenv
CLOUD_ORIGIN='https://cloud.example.com'
CLOUD_TRUSTED_PROXIES='127.0.0.1/32,::1/128'
```

`CLOUD_TRUSTED_PROXIES` 让登录限流按真实客户端 IP 计数；处在该地址范围内的进程（例如经 SSH 隧道访问回环端口的人）可以伪造客户端 IP。

## 当前部署及操作确认

管理员入口为 `https://192.168.0.111`，前端、Nginx 和后端均运行于该服务器。管理员操作不再填写、提交或记录 `reason`；仍保留操作人/类型/时间等基础审计、删除名称确认和强停风险确认。自定义实例使用平台外沟通、管理员直接下发流程。

2026-10-30 已发布无操作理由版本：静态文件 `/srv/101cloud/releases/20261030-no-reason`；后端备份 `/usr/local/bin/101cloud-backend.before-no-reason-20261030`。验证通过：后端 `go test -race ./...`（含隔离 MySQL 认证回归）、`go vet ./...`，前端类型检查、oxlint 及生产构建；本地浏览器对测试用户不填理由完成密码重置（204），线上登录并确认节点排空对话框无理由输入（取消，未执行排空）。本轮未连接其他服务器。

2026-10-30 菜单扁平化：去掉“管理”折叠父层，11 个管理菜单直接位于侧栏顶层；原 `/admin/*` 地址及 admin 权限保持不变。发布目录 `/srv/101cloud/releases/20261030-flat-menu`，仅部署至 `192.168.0.111`。

2026-10-30 已发布管理员直接下发版本：`/srv/101cloud/releases/20261030-admin-direct-only`。侧栏现在为 10 个顶层菜单，无申请审批入口，概览无待审批卡片。后端通过前向迁移 `20261030_80_retire_custom_requests.sql` 删除 `custom_instance_requests`、`residency_requests` 及 `instances.custom_reason`；已应用的旧迁移文件保持不变。旧申请的唯一已撤销测试记录随表删除，完整数据库备份为 `.111` 的 `/var/backups/101cloud/before-retire-approval-20261030.sql.gz`（0600）。旧程序及前端路径也保存在同目录；旧程序不能直接配合新表结构回滚，需先恢复兼容结构。

验证：后端 `go test -race ./...`、`go vet ./...` 通过，包含真实 MySQL 新安装及升级保留运行实例回归；前端类型检查、oxlint 与构建通过。线上确认八个申请审批接口全部 404、管理员直接下发接口仍校验参数（空请求为 422），直接下发表单正常加载但未实际创建新实例。既有自定义实例保持运行。仅操作 `192.168.0.111`。

2026-10-30 地址池范围编辑已部署到 `.111`：网络与 IP → 地址池 → 编辑，可修改 `range_start` / `range_end`。范围仅能落在原网段内，不能包含网关或排除任何已登记 IP；范围变更不会自动登记或探测 IP，也不会使已验证绑定失效。无需数据库结构迁移。发布目录 `/srv/101cloud/releases/20261030-poolrange`，后端备份 `/usr/local/bin/101cloud-backend.before-poolrange-20261030`。

验证：真实 MySQL HTTP 回归覆盖范围扩展、合法缩小、过期 ETag、倒序/越界/保留地址/网关冲突、登记地址保护和合并网关变更；后端 race 测试及 vet、前端类型检查/lint/构建通过。本地浏览器扩展范围保存成功，排除已登记 IP 被中文错误拒绝。线上仅查看已有地址池编辑字段，未代替管理员调整实际分配范围。

2026-10-31 可读错误改造已仅部署 `.111`：前端 `/srv/101cloud/releases/20261031-readable-errors-r2`，后端备份 `/var/backups/101cloud/backend-before-readable-errors-20261031`。准入失败返回具体资源维度、精确字符串用量/申请/上限/可用量和中文处理建议；地址错误显示提交地址、允许范围及字段；任务/步骤新增安全 `error` 对象，任务中心、列表、详情与失败实例统一展示；历史未进入阶段的任务允许 `phase:null`。不输出 SQL、凭据或原始上游错误。

新建实例会先只读检查实际存储池。仅有完整不存在证据、无派发记录/恢复历史、有效归属与租约、未计量未扣费等条件全部满足时，创建前失败才原子释放预留并标记失败；失败记录仍可见。任何不确定结果继续保留预留并要求管理员核对。旧任务 43 未重试、未释放、未改写。无需数据库结构迁移。

验证：后端 race/MySQL 回归与 vet、前端类型检查/lint/生产构建通过。隔离真实 HTTP 后端加固定 TLS 协议场景覆盖用户磁盘配额不足、实际 19 GiB/可用 15 GiB/申请 20 GiB 的同步拒绝，以及准入后容量变化导致的异步失败与原子回收（原生写操作 0 次）；这不是实际硬件创建测试。浏览器验证任务/实例明细与同步错误通知。线上验证明确中文地址拒绝通知及旧未知任务展示，未新增实例或改动配额、存储池容量。

2026-10-31 已仅在 `.111` 移除 Web 终端/串口/VGA 固定时长上限和倒计时。迁移 `20261031_90_console_no_fixed_expiry.sql` 删除控制台专用 `expires_at` 与相关索引，`CLOUD_CONSOLE_TTL` 已从运行配置移除；登录有效期与每账号最多 4 个并发控制台不变。退出/登录失效、停用/撤权、实例停止和网络故障仍可断开连接。前端发布目录 `/srv/101cloud/releases/20261031-console-no-ttl`。

升级前完整备份在 `/var/backups/101cloud/before-console-no-ttl-20261031.sql.gz`，程序和配置备份位于同目录。后端停止后已关闭旧进程遗留的开放控制台记录（不重开历史过期会话），既有实例状态未改。旧后端回滚需要先恢复控制台到期字段/索引，不能仅换二进制。

验证通过：后端 race/MySQL 回归（超过原 10 分钟的会话鉴权/单次连接、并发上限、权限撤销及登录失效关闭、新装与升级）、vet，前端类型检查/lint/构建。线上真实控制台返回 201 且无 `expires_at`，WSS 内执行只输出测试标识的 printf 命令并验证输出；测试连接随后关闭。界面标题已无剩余时间，测试登录已退出。

2026-10-31 删除后 GPU 回收确认修复已仅部署 `.111` 后端及节点代理，本次无需数据库迁移或前端重新发布。采样新增 `host_quality` 和 `collection_started_at`：独立保留宿主机进程/上下文/设备引用完整性，不再让其他停止实例的行级未知质量阻塞已删除目标；后端最多等待 95 秒，要求采样开始晚于本次原生删除确认、数据新鲜、完整枚举中目标不存在及明确完整的宿主机归属证据。未确认时仍保留资源，并返回中文 `gpu_reclamation_unverified`；任务阶段区分 `delete.native`、`delete.verify_reclamation` 和 `delete.finalize`。

已通过管理员 API 对账删除任务 49（实例 16，`1121`）及 54（实例 1，`backend-acceptance-116`），产生任务 55、56，全部成功。原生操作引用沿用既有日志，没有重发 DELETE；两实例已写入删除终态，IP `192.168.0.116` / `.118` 已恢复 available 并解除实例/节点绑定，共两条共享 GPU 分配已释放且 `release_verified=1`，活动 GPU 计量会话均结束。归还预算占用合计 3 vCPU、5 GiB 内存、8 GiB 磁盘；这不是此次新增物理磁盘空间。创建未知任务 43 保持原状。

验证：后端和代理 race 全套及 vet、真实 MySQL/TLS 删除与幂等对账回归通过；Linux 实际运行 monitor 边界回归通过（原有权限测试中 root 不适用的一个子用例跳过）。线上历史采样确认瞬时进程扫描竞争仍保持 fail-closed，后续完整样本可安全收尾；重复对账已成功任务返回 409 `task_not_unknown`。浏览器任务页显示上述四条任务成功，无运行时错误。数据库备份 `/var/backups/101cloud/before-delete-reclaim-20261031.sql.gz`，后端和代理旧二进制备份均在同目录，后缀 `before-delete-reclaim-20261031`。
