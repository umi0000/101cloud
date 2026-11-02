# 101cloud Frontend

基于 [Vue Vben Admin](https://github.com/vbenjs/vue-vben-admin) v5.7.0（MIT，见 [LICENSE](LICENSE)）的 `web-antd` 应用裁剪而来，只保留 ant-design-vue 版本。设计与后端契约见 [前端设计.md](前端设计.md)。

当前版本开放管理员与普通用户共用登录。管理员进入 `/admin/dashboard`；普通用户进入 `/instances`，侧栏仅有“实例管理”，当前只开放只读卡片首页。后端对 `/api/v1/admin/*` 继续强制 admin 角色。

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
  views/member/instances/* 普通用户实例卡片首页
  components/member-balance.vue 普通用户顶栏可用额度及额度详情浮层
```

`packages/`、`internal/` 为 Vben 上游代码，尽量不改，便于以后手动合并上游。

## 普通用户实例首页

- 登录及根地址按 `/me.user.role` 分流；登录回跳只允许当前角色的站内页面。退出或重新登录切换账号/角色会清理前一账号的状态。普通用户不挂载固定使用管理员接口的任务中心。
- `/instances` 使用本人接口，每页 20 条，支持运行状态、产品类型筛选、刷新和游标上一页/下一页。卡片展示实例名、ID、类型、组合状态、分配 CPU/内存/磁盘、状态过期及额度停机提示。GPU 分配通过 `/instances/{id}/gpu-allocations` 独立加载，按型号及分配状态汇总数量，不把历史释放/取消记录算作当前分配。
- 卡片复用 `OsIcon` 及已有本地系统 SVG，依据实例接口 `image_os_name/image_os_release` 选择真实来源镜像图标，停用/退役镜像不影响已有实例展示。GPU 的 `configured` 表示挂载配置，`observed` 表示进程上下文/设备引用；已配置但空闲时仍显示“已挂载”，不因 `observed=false` 误报未挂载。
- 规格下方显示“内网 IPv4”，读取本人 `/instances/{id}/network` 并仅展示已绑定的 RFC1918 私网地址。网络异步加载，失败保留旧值并提示，过期状态有标记；未绑定及仅预留地址不冒充可用地址，无地址显示“未分配内网 IPv4”。
- 顶栏通过 `/credits/account` 显示 `available_pt`（Pt），浮层展示 `remaining_pt`、`reserved_pt`、服务端停机提示及手动刷新；页面可见时每 30 秒更新。额度失败不显示为 0，已有值保留并明确标记更新失败；实例列表及 GPU 刷新失败同样保留上次结果并提示。
- 暂不开放创建、实例详情/操作、账单、通知和个人设置入口，不提供虚假的地域、到期/续费或名称/IP全量搜索。桌面多列、窄屏单列，沿用现有主题。
- 2026-11-02 已仅发布到 `192.168.0.111`，当前前端目录 `/srv/101cloud/releases/20261102-member-ipv4`；详情及回滚位置见文末发布记录。


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

2026-10-31 用户与配额页筛选栏重叠已修复，仅前端部署 `.111`：发布目录 `/srv/101cloud/releases/20261031-users-layout`，前一版本链接记录在 `/var/backups/101cloud/frontend-before-users-layout-20261031.link`。原因是 antd `Space wrap` 的负下边距（-8px）抵消了 `mb-3`，表头上移 4px 压住控件；改为与节点/网络页一致的 flex 工具栏，筛选项左侧、"创建用户"右侧。验证：类型检查/lint/构建通过；线上浏览器测得 1440px 宽度时控件与表头间距 8px，卡片收窄到 520px 换行时 12px，无运行时错误。

2026-10-31 已移除可用区概念，仅部署 `.111`：迁移 `20261031_95_remove_availability_zones.sql` 删除 `availability_zones` 表及节点/地址池/套餐的 `availability_zone_id`，地址池名改为全局唯一、网段重叠检查覆盖全部同族地址池；`/catalog/zones`、`/admin/zones` 已删除，各接口不再接受 `zone_id`。直接下发自定义实例必须选择节点，网络和 GPU 选项按节点过滤（`node_id`）。前端发布目录 `/srv/101cloud/releases/20261031-remove-zones`；完整数据库备份 `/var/backups/101cloud/before-remove-zones-20261031.sql.gz`，旧后端二进制和前端链接同目录。回滚需先从备份恢复数据库结构，不能只换二进制。验证：后端 vet/race/真实 MySQL 回归（含全新安装与升级保留节点/地址池/套餐）、前端类型检查/lint/构建通过；本地浏览器验证节点下拉禁用维护节点、选节点后仅列该节点网络；线上确认节点、地址池和运行中实例保留，`/admin/zones` 404，各管理页无可用区字样，下发表单可选节点，无运行时错误。

2026-10-31 登记镜像的代码和版本改为可选，仅部署 `.111` 前后端，无数据库迁移。留空时代码取指纹前 12 位、版本为 `1`，名称为“系统名称 系统版本（代码）”（超过 128 字符时回退为“代码 版本”）；填写时仍按原格式校验，名称仍为“代码 版本”。前端发布目录 `/srv/101cloud/releases/20261031-image-optional`，旧后端二进制 `/var/backups/101cloud/backend-before-image-optional-20261031`。验证：后端 vet/race 与新增默认值边界测试、前端类型检查/lint/构建通过；线上留空代码和版本登记了节点已有的 jammy 镜像（ID 2，`ubuntu 22.04 (4faaa23407ec)`），无运行时错误。

2026-10-31 政策与定价改为可直接编辑的配置档，仅部署 `.111` 前后端，无数据库迁移。去掉“新建草稿→发布/退役”：新建配置档立即生效（status=active）；点击即在抽屉内直接编辑并保存（PATCH 需 If-Match，同时更新对应来源表）；“设为平台默认”替换该类绑定；新增 `DELETE /admin/policies/:kind/versions/:id`，平台默认或仍被套餐、实例、额度账户、每日发放、GPU 或使用记录引用时返回 409 并列出引用数量。`publish`/`retire` 接口已删除。注意：编辑当前绑定的每日额度配置档会立即影响所有使用它的账户；定价改动只影响之后创建的实例（已有实例/计量会话保存当时单价）。前端发布目录 `/srv/101cloud/releases/20261031-policy-profiles`，完整数据库备份 `/var/backups/101cloud/before-policy-profiles-20261031.sql.gz`，旧后端二进制同目录。验证：后端 build/vet/race 全量（含真实 MySQL 配置档创建、编辑、切换、删除、引用拒绝）、前端类型检查/lint/构建通过；本地浏览器走通新建→编辑保存→设为默认→默认不可删→删除被引用配置档显示中文原因→删除未引用配置档；线上仅打开每日额度确认可直接编辑，未修改实际政策。

2026-11-01 按界面精简要求移除政策与定价页顶部“平台默认绑定”汇总卡片及其批量修改弹窗，删除独占组件与全量配置档预加载；配置档抽屉内的保存、删除、“设为平台默认”和列表默认标记保留，后端接口及已有绑定不变。仅更新 `.111` 前端，发布目录 `/srv/101cloud/releases/20261101-remove-binding-card`，旧链接记录 `/var/backups/101cloud/frontend-before-remove-binding-card-20261101.link`。类型检查、oxlint、生产构建通过；线上确认仅剩配置档列表卡片，打开配置档仍有默认切换/删除/保存按钮，无运行时错误，未修改真实政策。

2026-11-01 为 GPU 调度配置的评估窗口、最小改善幅度、重绑统计窗口、窗口内最多重绑次数、每节点最多并发、指标最大时效添加简短字段说明，使用已有 hint 展示方式；统计范围及百分点含义按后端实现说明，不改变参数值和调度逻辑。仅部署 `.111` 前端 `/srv/101cloud/releases/20261101-gpu-scheduling-hints`；类型检查、oxlint、生产构建通过，线上抽屉确认六项说明均显示，无运行时错误，未保存或修改实际政策。

2026-11-01 调整规格页面改为按真实剩余配额展示允许总量，仅部署 `.111` 前后端，无数据库迁移。新增用户/管理员 `GET /instances/:id/resize-limits`；与准入共用预留统计，排除当前实例，按用户剩余额度、节点剩余容量及自定义实例常驻预算取最小值，仍保留未知任务的资源占用。前端再与套餐范围取交集，磁盘不缩容，GiB 边界向内取整至三位小数；读取失败或范围不足时禁止提交，支持重试，提交仍由后端重新校验，实际存储容量检查不变。

验证：后端 vet、race 全量及真实 MySQL 回归通过（自身排除、同节点其他用户、停止标准实例、已释放/未知失败、常驻预算、超额归零、精度、所有权与只读）；前端类型检查、oxlint、生产构建通过。线上实例 19 显示 vCPU 1–8、内存 0.001–2 GiB、磁盘 4–5 GiB，接口数据与手算一致；真实浏览器断网后不可提交，联网重试恢复且保留输入，CPU 输入 9 后失焦限制到 8。未提交任何 resize，请求验收后实例仍为 2 vCPU / 1 GiB / 4 GiB。发布目录 `/srv/101cloud/releases/20261101-resize-limits`，数据库及旧后端备份在 `/var/backups/101cloud/`，前缀 `before-resize-limits-20261101` / `backend-before-resize-limits-20261101`。

2026-11-01 实例详情操作栏在 running/stopped 时显示“修改实例规格”和“销毁实例”，仅部署 `.111` 前后端，无数据库迁移。运行中修改规格须勾选重启确认，API `confirm_restart=true`，后端单任务执行正常停止→确认停止→调整规格→确认规格→启动恢复运行；停止实例调整后仍保持停止。运行中销毁须输入实例名、勾选强制关闭/永久删除风险，管理员 API 使用 `mode=force` 和 `confirm_data_loss_risk=true`，先强制停止并确认停止，再删除，继续使用删除后 GPU 回收门禁。运行中缩容在停止确认前按原值与目标值的逐项最大值保留 CPU/内存预算；旧计算区间不提前改为新规格结算，恢复对账不重放已完成阶段。

验证：后端 vet、race 全量真实 MySQL 回归通过（确认/权限、顺序、停止失败不删除、预留/计费快照、竞争准入、部分执行恢复）；前端类型检查、oxlint、生产构建通过。隔离 MySQL 与状态式 TLS 节点协议场景中，经 curl 触发真实任务，确认 stop→resize→start，及强停 stop→delete 后 IP/GPU/用量回收；这是协议场景，不是实际硬件测试。线上仅打开实例 18 的两类确认弹窗及实例 19 的停止态规格弹窗，未确认风险时按钮禁用，运行中有重启/强制关闭提示、精确名称确认保留；未提交任何线上生命周期操作，实例 18 仍运行且规格未改。前端目录 `/srv/101cloud/releases/20261101-running-actions`，完整数据库和旧后端备份在 `/var/backups/101cloud/`，分别为 `before-running-actions-20261101.sql.gz`、`backend-before-running-actions-20261101`。

2026-11-01 修复顶栏偏好设置齿轮点击无反应：自定义触发按钮原先覆盖默认插槽但没有绑定抽屉打开事件，现在通过现有 Preferences.open 暴露方法打开，并添加本地化 aria-label。仅更新 `.111` 前端 `/srv/101cloud/releases/20261101-preferences-open`，旧链接 `/var/backups/101cloud/frontend-before-preferences-open-20261101.link`。类型检查、包含修改组件的 oxlint、生产构建通过；线上真实点击验证抽屉打开、关闭、重新打开及主题即时切换，无运行时错误，验证后恢复浅色并退出登录。既有初始化配置优先于缓存的规则未改：刷新仍恢复配置指定的默认浅色；本次仅修复入口，不声称主题持久化行为改变。

2026-11-02 节点镜像目录已完整部署到 `.111` 前后端及 Host Agent：前端 `/srv/101cloud/releases/20261102-node-images`。登记支持节点已有本地镜像核验、通过节点 Agent 读取真实官方目录并下载远程镜像、批量选节点及逐节点项目/架构/cloud-init/指纹/GPU 兼容性配置；只有原生核验后的 ready 配置可供对应节点/项目创建或纳管。未知下载走原任务对账，模糊派发不重发；GPU 就绪始终是管理员声明，不自动安装驱动。当前支持的源为 `images.linuxcontainers.org`，Ubuntu OS 从其中真实目录选择，不暴露格式不兼容的 Canonical Cloud 源。

线上浏览器实际从 1895 个构件的目录选择 Alpine 3.22、容器、x86_64、cloud-init，通过“选择全部在线节点”批量入口提交，登记返回201，产生镜像3和任务76。Incus 原生证据确认新下载的私有镜像完整指纹 `04dbb9362a46ef57716981d224a47577d1dde1ad6c2101d2a517d375e2666992`，大小19.34 MiB、variant=cloud；任务持久保存操作ID及default项目，原生核验后ready。原有本地镜像1/2经迁移任务73/74就绪，cloud-init由真实cloud变体补齐；又通过浏览器“配置节点”对镜像2完成任务77的本地只读核验（202，无原生下载操作）。修复了旧properties往返管理的兼容问题及就绪后残留的缓存下载进度显示。

实例下发表单实测随节点/网络绑定请求 `images?node_id=1&project_name=default`，仅显示3个已就绪镜像；不存在的另一项目返回空列表。任务页正确显示无关联实例的“准备节点镜像”成功任务，浏览器无运行时错误，验收登录已退出。未提交实例创建、停止、删除或规格调整；实例18/19保持运行，实例15及未知任务43保持原状，所有既有实例的冻结指纹、架构和规格未变，旧原生镜像保留。

验证通过：后端及Host Agent `go vet ./...`、全量 `go test -race ./...`（后端启用真实MySQL/TLS回归，覆盖新装/升级保留、项目隔离、多节点差异配置与部分失败、模糊派发不重放和对账、创建/报价/纳管消费者）；前端类型检查、oxlint和生产构建；OpenAPI JSON及244处内部引用校验（147个操作）。线上只有一个管理节点，多节点和异常场景在隔离MySQL及两个独立固定证书TLS节点中验证，不声称进行了多台物理服务器下载。升级前完整数据库 `/var/backups/101cloud/before-node-images-20261102.sql.gz`（已校验gzip完整性）、旧后端 `backend-before-node-images-20261102`、旧前端链接 `frontend-before-node-images-20261102.link`，以及旧Agent `hostagent-before-node-images-20261102` 均在同目录；历史init.sql校验和不变，新增迁移 `20261102_00_node_images.sql` 已应用。

2026-11-02 增加用户端用的16个本地系统SVG资源，位于 `public/images/os/`；常用系统使用 Simple Icons 品牌主色，AlmaLinux 使用官网原版多色图标，来源、固定版本和许可保留于同目录 `LICENSE.txt`。`src/utils/os-icons.ts` 按系统名称匹配，忽略大小写及常见分隔符；各发行版本共用系统图标，未知名称使用 Linux 图标。`src/components/os-icon.vue` 使用 img 保留彩色，不使用 currentColor 遮罩。当前没有用户端业务页面，本次不新增业务页面、不接入管理员页面。类型检查、oxlint、生产构建通过，独立Vue组件验收页验证加载和别名，临时页已删除；线上16个SVG均200且XML有效，品牌颜色核对通过。仅发布 `.111` 前端资源 `/srv/101cloud/releases/20261102-os-icons-color`，管理员界面和后端逻辑不变。

2026-11-02 使用用户提供的 brill-logo.png 作为网站Logo，保留原图比例和透明背景；生成256px本地Logo、32/64px标签页PNG及含16/32/48/64px的favicon.ico。偏好覆盖仅设置 logo.source，保留已有布局、主题及网站名称；HTML图标链接附带版本标识以更新浏览器缓存。仅发布 `.111` 前端 `/srv/101cloud/releases/20261102-brill-logo`，旧前端链接 `/var/backups/101cloud/frontend-before-brill-logo-20261102.link`。类型检查、oxlint、生产构建通过；线上顶栏Logo实际加载且尺寸32×32，三个浏览器图标资源均200，页面无运行时错误。原始桌面图片未改动。

2026-11-02 普通用户实例首页已仅部署测试服务器 `192.168.0.111`。发布目录 `/srv/101cloud/releases/20261102-member-home`，`/srv/101cloud/current` 已原子切换；前一版本 `/srv/101cloud/releases/20261102-brill-logo` 保留，回滚链接记录在 `/var/backups/101cloud/frontend-before-member-home-20261102.link`。本次只更新静态前端，未更新后端、数据库结构或 Nginx 配置，未重启服务；上传压缩包 SHA-256 两端一致。

验证：生产构建通过；线上管理员真实登录及原有10个菜单正常，普通用户真实登录进入 `/instances` 且仅有“实例管理”菜单。零配额验收账号显示空列表及真实可用额度100 Pt（由既有每日发放任务自动创建账户）；新账号首次进入时账户尚未创建曾返回404，定时任务完成后顶栏自动恢复，未伪造0余额。普通用户请求 `/api/v1/admin/instances` 返回403，访问管理员页面被送回实例首页；未登录的本人接口401。所加载的33个脚本/样式/图片请求无失败；验收浏览器仅允许 `.111`，外部图标服务请求被拦截。验收账号 `deploy-member-home-20261102`（ID6）已停用，成员和管理员验收会话均已退出；未创建或操作实例、未调整既有配额或额度。实例卡片非空、分页和异常场景沿用此前隔离MySQL实测，不声称在线上创建了实例验证。


2026-11-02 修复普通用户实例卡片系统图标和 GPU 挂载展示，仅发布 `.111`。实例列表/详情新增 `image_os_name/image_os_release`，通过来源镜像只读投影取得，支持停用/退役镜像，无逐实例镜像查询；前端接回已有 `OsIcon`。`active + configured=true` 显示“已挂载”，不再把表示上下文/设备引用的 `observed=false` 误解为未挂载；未改监控、计费及 GPU 挂卸/回收门禁。

前端 `/srv/101cloud/releases/20261102-member-card-fix`；旧前端链接 `/var/backups/101cloud/frontend-before-member-card-fix-20261102.link`，旧后端 `/var/backups/101cloud/backend-before-member-card-fix-20261102`。后端二进制更新并重启，Nginx配置、数据库结构及Host Agent未改。后端全量race测试（启用真实MySQL）、vet、Linux amd64构建，前端类型检查、29条用例、oxlint及生产构建通过。

线上只读检查实例21/20的GPU仍为 `active/configured=true/observed=false`，实例归属和规格与发布前一致。以管理员验收会话在浏览器临时挂载已发布成员页组件，使用真实本人 `/instances` 数据验证Arch/Ubuntu图标加载、空闲GPU显示已挂载、已释放GPU不计入当前分配及预留GPU仍为待挂载；桌面及390px窄屏均完成截图检查。临时路由仅存在于验收浏览器内，未发布额外入口，未改普通用户账号或冒充其会话；未创建、启停、调整任何实例或执行GPU挂卸。


2026-11-02 普通用户实例卡片新增内网IPv4，仅更新 `.111` 前端 `/srv/101cloud/releases/20261102-member-ipv4`，旧版本链接 `/var/backups/101cloud/frontend-before-member-ipv4-20261102.link`。后端、数据库、Nginx和Host Agent未改，无服务重启及实例/网络写操作。类型检查、52条用例（含私网边界）、oxlint和生产构建通过。

实际浏览器临时加载已发布成员页组件，以管理员本人接口只读验收：实例20/19分别展示192.168.0.119/.118，仅预留的实例15地址.117未显示为已分配；390px窄屏无横向溢出，网络刷新503场景保留原地址并提示失败。临时路由及故障拦截仅存在验收浏览器，未发布额外入口、未改普通用户账号或真实网络配置。


2026-11-02 套餐配置补全：显式选择可用节点，按节点→网络→镜像约束目标项目和 GPU 就绪能力；新增空闲回收开关/时长/动作、自动均衡/运行中换卡/精确单价上限，以及实例用户动作和六项 GPU 策略编辑权限。查看、编辑和复制均保留完整配置（该次部署沿用的草稿/版本工作流已由下述独立启停与随时编辑替代）。仅用户实际变更节点、网络或类型时清理不兼容选择，载入配置不静默裁剪；虚拟机切换时清除并禁止 GPU 策略和授权。

类型检查、oxlint、生产构建通过；隔离 MySQL 配合真实后端 serve 与浏览器验证复制→修改时长和权限→保存草稿→发布→只读回显，节点移除后网络/镜像清空并禁用，虚拟机 GPU 开关和权限均关闭/禁用，明暗主题与390px窄屏无页面横向溢出。线上 `.111` 实际新建表单和真实节点下拉验证通过，无运行时错误；线上当前套餐为空，未留下验收套餐，也未提交实例操作。前端发布目录 `/srv/101cloud/releases/20261102-plan-configuration`，原前端链接备份 `/var/backups/101cloud/frontend-before-plan-configuration-20261102`。同步后端迁移与授权变更见 `../backend/README.md`。

## 套餐独立启停与随时编辑

套餐使用独立 `PlanStatus: active | disabled`，新建默认停用；不再使用套餐草稿、发布、退役或新版本操作。所有行均可查看、编辑和复制；多个套餐可同时启用，启停只影响当前套餐。镜像、初始化模板和政策的既有状态与工作流不变。

- `POST /admin/plans/{id}/enable {}`、`POST /admin/plans/{id}/disable {}` 替代套餐 publish/retire 接口，继续携带 `If-Match` 和 `Idempotency-Key`，无旧 API helper 别名。
- 所有字段（含代码和版本）始终可编辑；`PATCH` 不改变状态。复制是创建独立套餐：清空代码、版本默认 `1`，其余配置保留，代码/版本仍可修改。
- 启用及保存已启用套餐需完整通过引用、节点、网络、镜像和 GPU 策略就绪校验；停用套餐保存保留原有配置格式和引用校验。校验失败不改变状态或已有配置。
- 套餐修改仅用于后续创建。创建请求需携带报价返回的 `plan_revision`，套餐变化后重新加载与报价并确认；已有实例资源调整使用 `Instance.plan_snapshot` 中创建时冻结的资源和 `allowed_overrides`，不读取当前套餐范围、不发送当前套餐 `policy_versions`。custom 的快照为 `null`；标准实例缺失快照时禁止调整，不放宽范围。

类型检查、oxlint、生产构建通过；隔离 MySQL 配合真实后端进程与浏览器完成套餐A启用→复制独立套餐B→启用B，两个套餐同时出现在用户目录；分别在线编辑A规格与B的GPU超时，核对另一套餐revision不变。停用A后目录保留B，A仍可编辑并重新启用。390px窄屏无页面横向溢出。另以隔离已有实例验证调整弹窗使用冻结vCPU范围2–4，而非已编辑套餐的固定4核；内存/磁盘仍按创建时固定值锁定，未提交该界面中的实例操作。

仅发布 `.111` 前端 `/srv/101cloud/releases/20261102-editable-plans` 并同步后端迁移；线上已有启用套餐code1/v1保持active、revision2及原配置，编辑抽屉的代码、版本、名称和vCPU均可操作且显示“保存套餐”。线上只读查看后取消，未提交编辑/启停/复制/新建套餐，未新增测试套餐；8条既有实例的状态、冻结指纹/架构/规格、GPU策略和授权在发布前后逐项一致。页面无运行时错误，验收登录已退出。备份及后端回归证据见 `../backend/README.md`。
