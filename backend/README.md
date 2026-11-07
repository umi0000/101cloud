# 101cloud Backend

工作室实例管理后端，使用 Go、Gin、MySQL 和 GORM。提供 CPU 虚拟机、GPU 容器、积分计费、管理员自定义实例、监控和 Web 运维接口。

后端自身提供 **HTTP**，默认 `127.0.0.1:8080`。浏览器入口的 HTTPS 交给 Nginx/Caddy；Host Agent 仍使用独立的 HTTPS 和证书指纹认证。当前不提供 NAT 或端口映射。

## 任务中心强制停止（2026-11-07，未部署）

- 成员 `POST /api/v1/tasks/:id/force-stop`，管理员 `POST /api/v1/admin/tasks/:id/force-stop`，固定请求 `{ "confirm": true }`，沿用会话 CSRF、`Idempotency-Key` 与任务 `If-Match`。同步返回 **200 Task DTO + ETag**，不是新增异步停止任务；任务历史保留。`queued/running/waiting/unknown` 全类型可撤销，已终态幂等原样返回。
- 成员仅能停止本人主动发起的任务，不是所有 `user_id` 属于本人的内部任务。新任务记录可信请求发起人；历史任务从已提交响应与审计映射核实。欠费强制停机、后台清理及管理员代办不授予成员取消权，返回 `403 task_stop_not_allowed`；管理员覆盖全部类型。DTO 的 `can_force_stop` / `force_stop_blocked_reason` 提供能力，不按类型猜权限。
- 撤销任务租约代次、取消活动执行上下文，并同时撤销恢复任务引用的源任务和包装链；旧 worker 不能写终态或继续派发下一步。控制台准备/预连接同时关闭。不会仅从列表移除，也不调用外部 operation DELETE 冒充物理取消。
- 执行上下文仅在包含领域变更、幂等记录和审计的事务成功提交后取消；校验失败、版本冲突和事务回滚都不停止 worker。派发行锁只覆盖持久意图到 HTTP 实际写出请求的边界，不覆盖等待响应；迟到的 operation ID 只补记原 attempt 的 journal，不恢复租约或改写取消状态。
- 当前注册的16类任务均覆盖：`instance.create/start/stop/restart/resize/delete/import/reconcile/gpu/creation_cleanup`，`image.prepare`、`node.probe`、`node.drain`、`network.validate`、`monitoring.console.prepare`、`credit_enforcement_reconcile`。任务端点不按类型设豁免；镜像/节点/网络发布同样受租约围栏，控制台事务关闭会话并在提交后终止本进程预连接，其它进程通过持久状态撤销检查收敛。
- `cancelled + task_force_stopped` 表示**编排已停止**；`force_stop.external_state` 区分 `not_dispatched` 与 `requires_reconciliation`。已发出、operation ID 丢失或旧任务证据不足时，保留 native journal、GPU/IP、额度/费用预留和实例变更屏障；不退款、清欠、回滚或自动释放。`requires_reconciliation` 明确提示管理员待核对，不等于实例已关机。
- `instance.gpu` 仅在持久证据证明尚未派发时可安全回滚：只将本任务拥有的源卡 `releasing` 恢复为 `active`，只释放本任务拥有且未配置、未被观察到的目标 `reserved` 分配，并只结束目标的 `held` 用量；保留已 active/已配置目标卡、历史费用和账本。`unknown` 状态本身不是已派发证据；已派发或证据有歧义仍保留锁，不推测释放。
- 新 GPU worker 在执行只读前置检查前持久化 `gpu.preflight` fence；强停先撤销整个来源链租约，再检查 journal，不能只凭缺少 operation ID 放行。历史任务仅对实现可定位的精确归属前置错误保留受限 legacy 识别，文案变更不会自动扩大授权；任一来源存在 dispatch-started、operation ID 或未受 fence 保护的 journal 都继续保留屏障。目标预留逻辑撤销为 `cancelled`，不写物理释放验证；额度回收追加正常 release 流水，旧账不改。
- 实例 DTO 的 `active_task_status` 与同一行 `active_task_id` 配对，无选中任务时均为 null；保留屏障时可返回 `cancelled`，表示安全屏障而非仍在执行。
- 管理员强停欠费停机或额度核对任务后，后台保留原任务引用并标明待核对，不在下一轮自动新建同来源任务。欠款、余额和运行中的计费照常保留；管理员须显式调用 `/admin/credit-enforcements/:id/reconcile` 核对/恢复执行。这不是成员免欠费或暂停计费接口。
- 管理员可对保留证据的强停任务调用原 `/admin/tasks/:id/reconcile`，仅只读核对既有操作与资源身份，绝不重放原步骤。充分证据后解除变更屏障并记录核对完成，原取消历史不改。无 operation ID 的已派发歧义持续返回明确待核对，不能靠一次 404 或手动改状态跳过；管理员显式故障清理仍须原确认及安全核验。
- 历史 GPU 强停屏障也可通过上述管理员 GET-only 核对模式清除：整个已撤销来源链的持久证据证明派发前安全时，执行同样的本地回滚，无需向节点派发变更或伪造物理释放观测；证据不足仍走原有只读外部核对，不重放、不释放歧义或已派发资源。
- 升级需备份并停止所有旧后端，使用新增前向迁移 `20261107_20_task_force_stop.sql` 后启动新版；历史 `init.sql` 和迁移未改。数据库唯一变更键包含保留安全屏障的强停任务，防止其它进程绕过 HTTP 检查并发派发。此变更未部署。

本地验收（未部署）：独立 MySQL 8.4.13，后端各包真实数据库 race 回归通过（全量运行及派发边界旧断言修订后的 instances 整包重跑），`go vet ./...`、原生 arm64 与静态 Linux/amd64 内嵌构建通过；CLI 首次及重复迁移成功。真实 loopback HTTP → worker → 固定证书 TLS native 烟测覆盖 queued / 在途 / unknown，native PUT 仅1次、DELETE为0，迟到结果不能复活任务、IP及变更屏障保留；强制审计失败500回滚时运行上下文、租约代次和revision均不变，同原请求随后成功停止。消费者回归覆盖GPU/资金预留不释放、来源/跨账号拒绝、恢复链、并发、控制台连接及只读解除屏障。前端类型检查、59文件616项测试、生产构建通过；构建仍有既有 BigInt / 顶层 await 目标环境提示，测试仍有既有 sortablejs mock 提升警告。临时MySQL已正常停止，未操作线上任务或真实宿主实例。

本次 GPU 前置失败修复验收（未部署）：以原强停逻辑 overlay 执行新增 unknown 消费者回归先失败（源卡仍为 `releasing`）；修复后 queued / fenced-running / legacy-unknown、已配置目标保留及派发歧义回归均通过。真实 loopback HTTP → 生产 worker → 固定证书 TLS Agent 的归属不完整失败只发生 GET、无 `gpu.configure`，强停后原卡保留、目标预留逻辑取消、仅目标 held 用量结束，实例 `active_task_id/active_task_status` 均为 null 且下一停机请求被接受。管理员真实 HTTP `/admin/tasks/:id/reconcile` 经生产 worker 一次核对历史 cancelled 来源链即可安全解除，无需再次强停、重建实例或改库；本次无新增迁移。独立 MySQL 8.4.13 下各后端包 race 均通过（首次共享测试库未迁移导致 auth/monitoring/platform 缺表，迁移后仅重跑这三包通过），vet、OpenAPI 339 个内部引用、前端 60 文件 643 项测试、vue-tsc、前端内嵌生产构建及 arm64 / 静态 Linux amd64 后端构建通过；保留既有 sortablejs mock 提升和 BigInt/目标环境构建警告。隔离 MySQL、临时密钥、overlay 和构建资源已清理，未做线上操作。

## 用户自助注册（2026-11-06，未部署）

- 管理员入口：**用户与配额 → 注册设置**。配置注册开关、默认实例数、自定义实例数、vCPU、内存、磁盘、GPU配额，以及默认共享钱包额度政策。
- 用户入口：登录页的“注册账号”，或`/auth/register`。填写用户名、密码、确认密码和可选邮箱；注册成功返回登录页，不自动登录。
- 新装及升级均默认关闭注册。先创建已生效的额度政策，再配置默认值并开启；0配额表示禁止新增该项资源。自助注册在同一事务中创建额度账户并立即发放当前业务周期的完整每日额度，资金、每日记录或审计失败时全部回滚，不留下半成品账户。
- 修改默认值只影响后续自助注册；已有用户、管理员手工创建用户、实例和账本不变。注册只能创建普通用户，受同源CSRF、独立IP/用户名限流及数据库唯一约束保护。
- “当天”沿用额度政策的时区和每日重置时间：重置点前属于前一业务日，不提前领取未来周期。注册立即取得当前周期额度，但有效期取`注册时刻+daily_validity_seconds`与下一次每日重置点的较早者；每日额度不跨周期累加，同周期消耗后也不重复补满。每日金额为0时保持0余额；管理员手工创建账户仍从下一计划周期开始。
- **手动更新须执行新增迁移`20261106_00_registration.sql`**：按既有流程备份数据库及主密钥、停止旧后端，用新版二进制和迁移账户执行`migrate`后再启动；`serve`不自动迁移。历史迁移未改动。
- 前端已沿用内嵌构建流程：在仓库根目录运行`sh backend/build.sh`，跨平台构建时设置目标`GOOS/GOARCH`。不要只运行`go build`而漏掉最新页面构建。

本次仅在独立本地MySQL及真实应用/浏览器验证，未部署、未修改线上数据。

本地验收：MySQL 8.4.13下后端各包race回归通过（新增认证回归的字段扫描映射修正后重跑认证包通过），`go vet ./...`、原生及Linux/amd64构建通过；真实CLI首次迁移及重复迁移后，已有注册配置、用户、配额和额度账户快照保持一致。真实浏览器完成管理员配置保存/版本冲突恢复、匿名注册、重复用户名拒绝、新用户登录及默认额度政策绑定；默认vCPU从6改7后，老用户仍为6、新用户为7。注册期间关闭开关后不创建用户。未部署。

后续调整（未部署）：`migrate`支持`CLOUD_MYSQL_ADMIN_DSN`，无库名时继承业务库，错库或错端点拒绝，`serve`仍只使用业务账号。真实本地CLI已验证受限业务账号建表失败、ADMIN环境变量/配置文件迁移成功及重复迁移、运行连接身份隔离。注册即时额度已通过auth/billing真实MySQL race回归及HTTP验收：注册立即获得7pt，重启两次并经过后台轮询后仍仅一笔每日记录/额度桶/发放流水，失败回滚与0额度边界通过。本次调整不新增迁移；尚未应用注册功能迁移的环境仍须执行`20261106_00_registration.sql`。

## 每日额度固定重置（2026-11-07，未部署）

- 仅每日额度不结转：例如每日额度100pt、上周期剩余30pt、额外发放50pt，重置后为100+50=150pt，不是180pt。兑换和管理员额外发放保留原金额及有效期；欠款和已预留费用继续独立核算，不承诺所有来源的账户净余额恒为每日配置值。
- `daily_allowance_pt`是每周期重置额度，`grant_time`是政策时区的每日重置时间。`daily_validity_seconds`仍允许更早到期，但不能延长到下一周期以后；使用内嵌Go时区数据，按日历处理夏令时。跨过的已结束周期不补成可用余额，同周期重试/重启不重新补满。
- 上周期未用额度过期，历史发放和账本保留；预留部分只能结算原所属预留，过期后释放或创建失败退款不会重新变成可用额度。不会把兑换和额外发放一起清零，也不免除欠款。
- **升级须先备份、停止所有旧后端，再用新版执行`migrate`**。新增`20261107_00_daily_reset.sql`记录每日桶是否已按新规则处理；`migrate`及`serve`在开放HTTP/启动worker前按账户锁、分批处理旧每日桶，收紧到期时间并记过期流水，绝不延长旧有效期。不要让旧程序继续写入。
- 处理覆盖停用账户和已消耗完的每日桶，防止之后退款复活旧有效期；再次执行不会重复扣减或受后续政策时钟修改影响。历史桶缺少发放记录、引用不一致，或保留的来源政策日界线不能解释历史时间时明确报错并阻止启动，不能删除账本绕过。批次已提交的处理可安全重试；回退旧程序不自动恢复已清除的结转额度。

本次没有部署或修改线上余额。当前注册即时发放规则以本节为准，取代此前注册首笔可跨周期重叠的说明。

本地验收：隔离MySQL8.4.13下后端各包race回归、`go vet ./...`通过；真实CLI把旧30pt结转+当天100pt+额外50pt从180pt归一化为150pt，额外桶逐字段不变，账本新增expire -30而不删除旧流水。另用实际时钟跨越配置重置点：注册100pt、消耗70pt、额外发放50pt，重置前80pt、重置后150pt；再消耗80pt后保持70pt，重复migrate并两次重启的桶/每日记录/账本快照完全一致。前端586项测试、类型检查、生产构建通过，浏览器验证每日重置字段及375px抽屉完整换行。未部署。


GPU采样正常空状态传递修复（未部署）：指标原因allowlist保留 `gpu_utilization_no_samples` 与 `instance_not_running`，使新版Agent的无活动采样、停止实例状态可由前端准确解释。只增加安全原因码，不改历史数值、归因、计费或释放授权，无迁移。相关monitoring真实MySQL/race回归和vet通过；上线应同步Agent、后端及前端。

上述原因码传递已随Agent和前端发布 `.111` `/srv/101cloud/releases/20261105-gpu-empty-sampling`，后端SHA256 `dc07ff400aa2c0fd108e89a9a271943fa3efc350ab4f6997c968f41b1761c7ee`。真实实例42的最新GPU采样已持久化no_samples原因、进程/引用/显存0及SM NULL。ready正常，两服务active，无迁移或实例/GPU操作；数据库单事务备份、旧程序及配置位于 `/var/backups/101cloud/20261105-gpu-empty-sampling/`。

2026-11-05 成员实例详情配套接口已实现（未部署）：用户和管理员 `GET/PUT /instances/:id/gpu-policy` 返回只读 `gpu_binding: null | {policy_version_id,model,access_mode,gpu_hourly_pt}`，仅按实例冻结的 GPU 定价绑定解析，不依赖当前套餐或已有分配；没有绑定时返回 null，异常绑定返回既有错误，不自动回退。无数据库迁移、授权语义不变。隔离 MySQL 回归覆盖套餐修改后原绑定、无分配读取、本人和他人隔离、无绑定与异常绑定；真实本地 Router 浏览器完成策略保存及卸载任务受理，不声称执行物理 GPU worker。

上述配套接口已随成员详情发布到 `.111`，发布目录 `/srv/101cloud/releases/20261105-member-instance-detail`，回退资料 `/var/backups/101cloud/20261105-member-instance-detail/`；无迁移。新程序SHA256 `6e81ac1e4f900a1f2a48349360fff33d4b26da8fdd77a089c722add113a096b4`，服务active且HTTPS ready正常。只更新后端和静态前端，未变更Host Agent或Nginx配置，未执行生产实例变更。

2026-11-04 已部署 `.111`：确认实例停止后，CPU、内存、磁盘、GPU全部停止计费；GPU保持绑定并继续占用容量，不自动解绑或释放。再次确认运行后恢复全部应计费资源，磁盘free和admin_exempt仍豁免。停止失败、状态未知及节点离线不能提前视为停止；offline政策只处理未确认停止的离线区间。停机前费用保留、不追溯退款，未交付创建失败退款规则不变。

2026-11-05 已将移除 GPU 空闲利用率/显存百分比配置发布至 `.111`，前后端目录 `/srv/101cloud/releases/20261105-remove-gpu-idle-thresholds`，已执行前向迁移 `20261105_00_remove_gpu_idle_thresholds.sql`。只清理 gpu-idle 政策两项 JSON 字段和 idle_policy_versions 对应来源列，保留 CPU CHECK、其他政策及空闲超时/动作/开关；受影响公开政策 revision 增加以使旧 ETag 失效。政策 ID 5、12 已清理，实例及套餐的状态/规格/登录凭据哈希/策略绑定和实例 GPU 配置基线一致。固定的完整新鲜归属证据、零进程/零设备引用/零显存安全要求保持不变；历史 init.sql 和已应用迁移未改。

验收：隔离 MySQL 8.4.13 下全量 `go test -race -p 1 ./... -count=1`、vet、原生及 Linux/amd64 构建通过，涵盖新装/升级/重复迁移、政策数据保留和14类不安全证据清空闲置窗口。实际本地后端 CLI migrate 与 HTTP 创建/读取/编辑七字段配置成功，旧阈值字段提交返回422；OpenAPI 321个内部引用有效。前端政策测试9项、类型检查、构建，以及线上静态页面配合隔离 API 的管理员政策新建/编辑、管理员 GPU 安全规则、成员首页进入 GPU 配置和390px无溢出验证通过；没有执行生产实例或GPU操作。现有成员GPU组件9处非空断言导致oxlint未通过，未在本次修改或重跑确认。

完整全库（含事件/存储过程/触发器）、旧程序、配置、原前端入口及验收证据备份位于 `/var/backups/101cloud/20261105-remove-gpu-idle-thresholds/`，本机副本 `/Users/nagisa/developer/101cloud-backups/20261105-remove-gpu-idle-thresholds/`；gzip及两端SHA256一致。后端已重启且HTTPS ready正常，Agent与Nginx未改。恢复旧后端必须配套恢复迁移前兼容表结构及政策字段，不能只替换旧二进制；数据库恢复须考虑备份后新增业务数据。

新增前向迁移 `20261104_10_stopped_resources_free.sql`：将credit来源和公开credit config中旧存储模式转换为running_only，并收窄数据库CHECK为running_only/free（旧宽松CHECK无需删除）；活动宽限事件快照只同步存储模式，原deadline与其他选择保持。套餐/实例持有政策ID，plan_snapshot与usage_sessions.config_snapshot只存资源，不嵌入存储规则，无须改写。历史完成事件、账单/流水、价格和账户余额不修改。旧历史迁移保持原样。

结算前按持久观测对齐计费：近期且节点在线的 stopped 结束全部实际计费会话；running 恢复计算、非free磁盘及仍为active/configured的GPU。旧会话已结算部分不倒扣。观测过期但节点在线时，先在财务事务外只读查询原生实例，以项目/name/UUID核对身份；事务内复核版本、归属、更新时序及无在途变更，才刷新状态。查询失败、异UUID、未知状态、离线或并发变更都不冒充停止。独立MySQL实跑验证三类运行会话、停机0活跃计费且跨后台周期余额不变、GPU仍绑定、恢复及重复running无重复会话；完整MySQL回归、相关竞态测试、vet与编译通过。前端政策编辑器实查无保留磁盘收费选项，当前仅running_only/free。

本次发布目录 `/srv/101cloud/releases/20261104-stopped-resources-free`，数据库、配置、程序与原前端链接备份位于 `/var/backups/101cloud/20261104-stopped-resources-free/`。停服备份后使用新版内嵌 migrate 执行数据转换，健康接口ready且发布二进制SHA256与构建一致。真实账户umi已停止实例27/28无active计费会话，跨12秒多个结算周期的六类历史会话账单笔数、累计应收、last_to完全不变；未改旧账、未解绑资源或操作实例启停。运行自定义实例24仍admin_exempt。原qwe在本次发布前已不在未删除实例中。

实例调整容量预览：`GET /api/v1/instances/:id/resize-limits`（管理员使用 `/api/v1/admin/instances/:id/resize-limits`）按所属用户配额、节点可分配容量和 custom 常驻预算返回此实例可调整到的总资源上限。响应 `data.max_resources` 的 `vcpus` 为整数，`memory_bytes`、`disk_bytes` 为十进制字符串。聚合排除此实例；与正式准入共用预留统计，保留未知状态占用，仅释放有明确安全标记的创建失败预留；停止的 elastic 实例只在节点层释放 CPU/内存，不释放磁盘。接口只读、不查询 Agent、不报价、不预留或修改配置；所属用户未启用、配额缺失、实例数量超额或节点未验证/离线/资源观测过期时返回错误，不能视作无限容量。套餐范围和磁盘不可缩小约束仍适用，最终 POST 在原有锁下重新准入。MySQL 回归使用 `CLOUD_TEST_MYSQL_ADMIN_DSN`（需要创建/删除隔离测试数据库权限）；未设置时跳过。

实例 resize/delete 由后端单个持久化任务编排，客户端不得串联 stop/resize/start 或 stop/delete。resize 的 `confirm_restart` 可选，但运行中必须为 `true`；所有非管理员elastic/custom实例须拥有resize权限，运行中还须stop/start权限。运行中正常关机确认后改配置、确认配置后启动，原已停止实例保持停止。任务保存 `restart_after_resize:true`、`previous_resources` 和期望 running，步骤为 `state.stop → resize.native → resize.confirmed → state.start`；进入启动意图后恢复不得重放早先停机或配置步骤。套餐、配额、节点/常驻预算、resize-limits 和磁盘禁止缩容约束不变。

未成功交付的创建失败会自动退役：新增迁移 `20261104_00_failed_creation_cleanup.sql` 持久记录首次失败、原任务及清理状态，不修改已应用历史迁移。列表和成员详情/网络/GPU/控制台/监控立即不可见，仅原创建任务保留安全 `creation_cleanup` 摘要及原错误；管理员可按ID核对内部退役行。后台先只读收敛原操作、验证项目/name/UUID/平台归属，再复用stop/delete和删除后的独立GPU采样；断连或歧义保留unknown与库存占用，不重放未知mutation。仅从未交付的create适用，成功任务/create.verify/checkpoint和后续生命周期错误均保护。失败标记同事务启动分批冲正，等待物理清理期间不收费；过期bucket不复活，欠费取消、预留释放，财务历史不硬删。此代码变更不部署、不操作线上旧实例。

退役后普通排队 stop/delete 任务同样失去执行权限；只有核对完成的内部清理任务可把原任务身份传给嵌套停止/删除。隔离 MySQL 下已验证并发冲正、分批恢复、过期来源、其他实例欠费保护、GPU 采样与异身份拒绝；独立 HTTP/worker 烟测使用模拟 TLS Incus，创建失败后 stop/delete 各一次，成员列表为空、详情404、活跃计费及未释放预留均为0，原任务保留错误并返回 completed/refunded。真实线上旧实例仅只读调查，未执行回收或退款。

2026-11-04 已部署 `.111` 并执行上述内嵌迁移，备份位于 `/var/backups/101cloud/20261104-failed-creation-cleanup/`；本段发布记录取代前述仅源码验证阶段的“未操作线上”状态。旧GPU配置记录的operation UUID按原项目解析为原生操作路径，相关隔离MySQL竞态回归通过。实例29的历史失败已确认清理，全部计费会话结束，GPU释放核验为true、IP解除；81.381546 Pt已通过14304笔refund原流水冲正，未退款扣费、未取消相关账单和held预留均收敛为0。结果不明且无明确失败证据的历史任务保留，未盲删。

删除必须精确确认实例名并提交 `delete_data:true`。运行中删除仅管理员可用，另须 `mode:"force"`、`confirm_data_loss_risk:true`；强停可能丢失未保存数据。同一持久化任务保存 force=true，确认强停后才向 Agent 发 DELETE，确认删除后获取新鲜 GPU 实际占用证据才释放分配。超时/未知保留预留并核对记录操作，不盲目重放破坏性步骤；普通用户仅可删除已停止实例。

管理员故障恢复删除：`DELETE /admin/instances/{id}`（亦支持 `POST /admin/instances/{id}/delete`）提交 `cleanup_stuck:true`、`mode:"force"`、`confirm_data_loss_risk:true`、精确 `confirm_name`、`delete_data:true`，可接管操作中/pending/unknown/陈旧实例并返回真实 task_id。事务撤销旧任务/租约、保留任务步骤和审计；原生派发行锁覆盖持久意图到 HTTP 实际写出请求的边界，迟到 operation_id 只追加原 journal，防止迟到 CREATE/START 被当作未派发而重放。先核实/取消并等旧原生操作终态，再确认目标缺席；最初已不存在也成功且不发 stop/delete。此显式清理以目标缺席为准，不等主机 GPU 指标；普通删除严格校验不变。财务/IP/GPU 释放一次，保留 deleted_at 墓碑及所有账本历史，不退还历史消费。丢失提交编号/旧版歧义 journal 公开 cleanup_native_submission_unknown，需从可信原生日志恢复编号后协调当前清理，不盲目重放旧创建或启动。

套餐配置：`PlanCreate`/`PlanPatch`和`Plan`包含`node_ids`、`gpu_policy`、`allowed_user_actions`、`gpu_user_permissions`。node_ids须非空且不重复，网络覆盖全部所选节点，镜像匹配全部节点/网络项目。启用及active编辑要求节点已验证且active/online、网络已验证、项目镜像ready（提供GPU型号还须gpu_ready）。报价/创建node_id若提供必须匹配所选网络节点并在套餐内。

gpu_policy使用idle.enabled/timeout_seconds/action和rebalance.enabled/allow_live/max_hourly_pt；超时及动作遵守绑定gpu-idle政策，GPU价格上限须数值相等（非负DECIMAL(20,6)字符串），报价/创建即使0卡也受上限约束。新套餐默认空闲/均衡/在线迁移关闭、用户动作start/stop/restart/console、GPU编辑权限{}；显式[]/{}不补全授权。VM不得启用GPU策略或授予GPU动作/true权限。套餐创建disabled，可任意多个独立active；任意状态可编辑含code/version（组合唯一），PATCH不改status且省略字段保留。POST /admin/plans/{id}/enable和/disable替代套餐publish/retire，要求If-Match和Idempotency-Key，active无效编辑原子回滚。模板/政策发布流程不变。

弹性实例准入复制套餐策略/动作/权限，rebalance.max_hourly_pt使用本次准入请求上限，与实例唯一GPU上限一致；新套餐版本不重写既有实例。全部非管理员elastic/custom实例的生命周期、GPU变更、GPU策略及console按实例allowed_user_actions逐次授权，只有管理员可绕过动作列表，不绕过安全/容量/确认要求；改显示名不属于这些动作。GPU策略allowed_ranges.editable还与六项gpu_user_permissions相交（idle、idle_timeout、idle_action、rebalance、allow_live、max_hourly_pt），缺失键false；PUT对变更字段重新授权。管理员实例GPU策略接口可明确替换动作/权限，[]/{}可撤销授权。

报价返回plan_revision，请求可选匹配；创建必须提交报价修订号，不匹配409 plan_changed且无副作用。20261102_20迁移为既有elastic冻结plan_snapshot={revision:字符串,vcpus:整数,memory_bytes:字符串,disk_bytes:字符串,allowed_overrides:对象}；新建同形，custom为null，resize读取冻结边界，套餐后续编辑仅影响未来创建。迁移不修改实例revision/state/config，孤儿elastic实例使迁移失败。

Instance DTO暴露plan_snapshot（custom为null）。resize仅接受resources及可选confirm_restart，不再接受policy_versions；使用实例冻结价格/政策和上述资源边界，不因套餐停用或改价而改用新套餐。

`20261102_10_plan_configuration.sql`在节点镜像迁移之后新增plan_nodes及套餐三个JSON配置列；节点从已有网络关联去重回填，不移动实例。旧套餐关闭GPU策略，容器保留旧完整动作/六项true编辑权限，VM只保留非GPU动作/空权限。既有elastic实例只修复对应SQL NULL或JSON null字段为旧兼容默认；保留已存策略（含已启用设置）、显式空数组/映射和其他非null值，不从当前套餐重新复制，GPU策略缺失时使用原实例价格上限。历史init.sql/旧迁移不改动，按前向迁移顺序升级。此处仅说明代码与迁移契约，不表示本次套餐配置扩展已部署或验证。

计费欠费恢复由持久化的 `credit_accounts.settlement_pending` 标记驱动，使用账户锁按批次处理；每个事务最多32笔欠费和有界资金来源，提交后释放锁，进程重启后从数据库继续。管理员发放、兑换和每日额度只入账并安排补扣，不在请求内等待全部历史欠费。已有预留（包括已过期桶）只支付其所属会话；未预留额度必须在实际后台抵扣时有效，不会延长过期时间。极短有效期的额度可能在补扣前过期，请勿将“已安排”视为保证偿债。没有可支付资金时停止安排；`settlement_pending=false` 不代表欠款已还清。

管理员额度调整返回真实已提交的 `CreditAdjustmentResult`：发放/扣减金额、额度桶ID/过期时间、提交时未偿欠款、净余额、预留、revision及 `settlement_pending`。发放请求不补扣历史账单，因此 `settled_debt_pt` 为0；后台完成后通过账户详情查看当前欠款，不能把提交快照当实时状态。账户及调整结果统一按 `remaining_pt = 内部剩余额度 - outstanding_debt_pt`、`available_pt = 内部剩余额度 - reserved_pt - outstanding_debt_pt` 返回固定6位精确金额；部分发放立即使负余额趋近0，不重复扣除欠款或预留。历史欠款需要的资金在后端准入、报价、预留、后续使用扣款和负数调整中保护，不能趁后台未完成重复消费；幂等重放仍返回原入账快照，刷新失败不能重新发放。

## 1. 部署前准备

推荐使用 Linux 服务器，准备：

- MySQL 8.4 LTS，以及独立数据库和数据库用户。
- 构建环境 Go 1.25 或更新版本、Node.js 22.18+ 或 24、pnpm 10.33.4，以及已安装的前端依赖；也可以在其他机器构建后上传二进制。
- Nginx 或 Caddy，以及浏览器入口使用的域名和 HTTPS 证书。
- 已安装、初始化好的 Host Agent。后端不会通过 SSH 自动安装 Incus、驱动或修改宿主网络。

后端可以与节点部署在同一台机器，也可以单独部署。服务器之间必须能访问 Agent 的管理端口。

## 2. 构建程序

在完整仓库中构建，不能只复制 backend 源码：go.mod 引用了相邻的 host-agent 客户端包。构建机需要 Go 1.25+、Node.js 22.18+ 或 24，以及 PATH 中可用的 pnpm 10.33.4，不要求 Corepack。运行服务器不需要 Node.js、pnpm 或前端源码。

### 2.1 首次准备依赖

从仓库根目录执行（首次构建或前端依赖锁文件变化后执行）：

```bash
npm install -g pnpm@10.33.4
cd frontend
pnpm install --frozen-lockfile
cd ../backend
go mod download
```

前端使用仓库 `packageManager` 指定的 pnpm 版本。上述步骤需要能访问依赖源；`build.sh` 直接调用 `pnpm`，不安装 pnpm 或前端依赖，因此应先完成依赖准备。Go 构建缺少模块时仍可能访问模块代理。

### 2.2 编译当前机器版本

以下命令从仓库根目录执行；如果已在 `backend` 目录，直接执行 `./build.sh`：

```bash
cd backend
./build.sh
```

脚本可从任意工作目录调用，先用 Vite production 模式在临时目录构建真实前端，再替换 `internal/webui/dist/`，最后编译 Go。失败的前端构建不覆盖旧 bundle，成功构建不会残留已删除的资源；生成目录不提交版本库。默认输出 `backend/bin/101cloud-backend`，额外参数透传给 `go build`（放在 package 前），例如 `./build.sh -o /tmp/101cloud-backend`。不改变 Vite 本地开发配置。

### 2.3 交叉编译 Linux 服务器版本

以下命令均在 `backend` 目录执行，可在 macOS 构建后上传至 Linux。Linux amd64（当前 `.111` 服务器）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ./build.sh
```

Linux ARM64：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 ./build.sh
```

两条命令默认都写入 `backend/bin/101cloud-backend`，后一次构建会覆盖前一次。需要同时保留不同架构时指定输出文件：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ./build.sh -o bin/101cloud-backend-linux-amd64
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 ./build.sh -o bin/101cloud-backend-linux-arm64
```

前端资源与 SQL 迁移均嵌入同一个二进制，运行不依赖工作目录或磁盘静态目录，不提供磁盘资源回退。发布只需替换对应架构的二进制，不再单独上传前端；仍需 MySQL、运行配置和原 master key，Host Agent 仍独立安装和更新。构建及启动不会自动执行数据库迁移，应按升级步骤显式执行 `migrate`。首次切换内嵌前端时，先安装新二进制，再按[第9节](#9-配置-nginx-反向代理)替换 Nginx 配置。

### 2.4 仅构建前端资源与运行测试

首次检出后直接 `go test`/`go build` 会因缺少嵌入资源失败；先准备 bundle，再测试：

```bash
./build.sh --frontend-only
go test ./...
```

此为新的构建/部署方式，当前线上尚未切换到内嵌前端；历史发布记录中的独立静态目录保留为当时状态，不代表本次已部署。

本地已验证统一脚本可从仓库外调用，默认原生构建及 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` 构建均成功，重建清除旧资源哨兵。实际 `backend` CLI 在无前端目录的临时工作目录完成隔离库迁移及serve，浏览器直接加载Go提供的登录页与深链接，366个真实资源HTTP字节与构建文件一致；缺失JS404、未知API JSON404、不支持API方法405、HEAD及缓存行为通过。完整真实MySQL `go test -race -p 1 ./... -count=1` 和 `go vet ./...` 通过。未部署线上、未改变当前Nginx或业务数据。

### 2.5 构建命令行参数

```text
./build.sh [go build 参数...]
./build.sh --frontend-only
```

| 参数 | 用途与限制 |
|---|---|
| 不传参数 | 先构建前端，再编译当前平台后端，默认输出 `bin/101cloud-backend` |
| `--frontend-only` | 只生成 `internal/webui/dist/`，不编译 Go；必须是第一个且唯一参数 |
| `-o <路径>` | 指定二进制输出路径；相对路径以 `backend` 目录为基准，不是调用脚本的工作目录 |
| `-ldflags '<参数>'` | 透传 Go 链接参数，例如 `-ldflags '-s -w'` 去除符号表和调试信息以缩小体积，但降低原生调试能力 |
| `-tags '<标签>'` | 透传 Go 构建标签，只有确实需要启用对应条件编译代码时使用 |
| `-race` | 启用 Go 数据竞争检测，用于调试；要求支持的平台和 C 工具链，不能与 `CGO_ENABLED=0` 组合 |
| `-v` | 输出参与编译的 Go 包；不控制前端构建日志 |
| `-p <数量>` | 限制 Go 构建并行任务数，例如内存受限机器使用 `-p 1` |

除 `--frontend-only` 外，参数原样传给 `go build`。脚本已指定 `-trimpath` 和入口 `./cmd/backend`，不要再追加包路径，也不要将 Vite 参数传给脚本。其他 Go 参数查阅 `go help build`；脚本没有独立的 `--help`/`--version` 选项。

`GOOS`、`GOARCH`、`CGO_ENABLED` 是环境变量，放在命令前，不是 `--os`/`--arch` 等命令行选项。Linux amd64 精简构建示例（在 `backend` 目录执行）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ./build.sh \
  -o bin/101cloud-backend-linux-amd64 \
  -ldflags '-s -w'
```

### 2.6 二进制运行子命令

构建参数不适用于运行程序。二进制接受一个子命令，以及可选的 `-c` 配置文件参数（放在子命令之后）：

```text
101cloud-backend install [-c /path/to/backend.env]
101cloud-backend migrate [-c /path/to/backend.env]
101cloud-backend serve [-c /path/to/backend.env]
```

| 子命令 | 行为 |
|---|---|
| `install` | 首次空库安装：创建数据库、执行迁移、创建受限业务 MySQL 账号；需要额外的 `CLOUD_MYSQL_ADMIN_DSN`，已有表或同名账号时拒绝覆盖 |
| `migrate` | 对 `CLOUD_MYSQL_DSN` 指定的业务库执行嵌入 SQL 迁移；优先使用 `CLOUD_MYSQL_ADMIN_DSN` 认证，未设置或为空时回退业务 DSN（此时业务账号必须有迁移权限），不启动 HTTP 服务 |
| `serve` | 始终只使用业务 `CLOUD_MYSQL_DSN`，忽略 `CLOUD_MYSQL_ADMIN_DSN`；检查迁移状态，必要时创建首个管理员，启动 HTTP 服务（内嵌页面、API、WebSocket）和后台 Worker；不会自动迁移 |

运行配置支持 `CLOUD_*` 环境变量和显式指定的文件，详见[第6节](#6-配置环境文件)。已设置的环境变量优先于文件（即使环境变量值为空）；未指定 `-c` 时保持原环境变量方式，不自动查找 `.env`。程序不支持 `--port`、`--config`、`--help` 或 `--version`；错误参数、配置文件不存在或格式错误时非零退出。不要把数据库密码写成命令行参数。

手工运行示例（运行用户必须能读取文件，与 systemd 二选一，不要重复占用端口）：

```bash
/usr/local/bin/101cloud-backend serve -c /usr/local/etc/101cloud/backend.env
```

首次安装、升级所需的数据库权限与凭据处理见[第7节](#7-自动初始化数据库及首个面板管理员)；日常 `serve` 仍使用受限业务账号，不保留安装用管理员凭据。

## 3. 创建运行用户和目录

以下以 Ubuntu/Debian、服务用户 `101cloud` 为例，需要管理员权限：

```bash
sudo useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin 101cloud
sudo install -d -m 0755 /usr/local/bin
sudo install -d -m 0750 -o root -g 101cloud /usr/local/etc/101cloud
sudo install -m 0755 bin/101cloud-backend /usr/local/bin/101cloud-backend
```

如果用户已经存在，不必重复创建。

部署路径统一为：二进制 `/usr/local/bin/101cloud-backend`，配置目录 `/usr/local/etc/101cloud/`。systemd服务单元仍放在系统标准目录 `/etc/systemd/system/`。

## 4. 准备MySQL服务，不需要手工建库建用户

首次部署只需MySQL 8.4已经安装、启动，并有可用于初始化的MySQL管理员连接。目标业务库可以尚不存在，也可以已经创建但完全没有表。

后面的 `install` 命令会自动创建数据库、执行全部建表迁移、创建业务MySQL用户，并授予该库的SELECT/INSERT/UPDATE/DELETE权限，再用业务用户验证连接。业务用户名、密码和库名取自CLOUD_MYSQL_DSN，不会自动生成或修改已有账号密码。

MySQL管理员需有建库、建表/迁移、创建账号及授权的权限，并能检查mysql.user。管理员凭据仅用于安装进程，不能放进长期运行的systemd环境文件。普通服务不使用root/DBA。

**install仅用于首次空库安装。** 库内已有任何表或同名业务账号已存在时，明确拒绝覆盖；失败留下的部分DDL需要检查后修复，不自动删库或删用户。已有安装升级使用migrate，而不是重复install。

## 5. 生成主密钥

只在首次部署生成一次：

```bash
sudo sh -c 'umask 077; openssl rand -base64 32 > /usr/local/etc/101cloud-panel/master.key'
sudo chown 101cloud:101cloud /usr/local/etc/101cloud-panel/master.key
sudo chmod 600 /usr/local/etc/101cloud-panel/master.key
```

主密钥用于保护节点凭据等数据，文件必须为 `0600`，且服务用户可以读取。**与数据库一起安全备份，不要每次部署重新生成，也不要提交 Git。** 丢失后无法正常解密原有凭据。

## 6. 配置环境文件

创建 `/usr/local/etc/101cloud/backend.env`：

```dotenv
# 必填：浏览器访问面板的外部地址，用于同源安全检查和 Cookie 安全属性。
# 改为实际域名或 IP，例如 https://192.168.0.111；不是 Agent 地址。
# 不带路径、末尾斜杠或默认端口 :443；非默认端口需保留，例如 :8443。
CLOUD_ORIGIN='https://cloud.example.com'

# 后端 HTTP 监听地址，默认 127.0.0.1:8080。
# 与 Nginx 同机时保持回环地址，Nginx 负责 HTTPS；不要无保护地暴露到公网。
CLOUD_LISTEN='127.0.0.1:8080'

# 必填：MySQL 连接串，格式 用户名:密码@tcp(数据库地址:端口)/数据库名。
# serve 使用受限业务账号；migrate 可另设 CLOUD_MYSQL_ADMIN_DSN，无须修改此项。
# 此项含密码，不要公开或提交 Git。
CLOUD_MYSQL_DSN='101cloud_app:替换为运行账户密码@tcp(127.0.0.1:3306)/101cloud'

# 必填：主密钥文件路径，用于加密/解密节点凭据等敏感数据。
# 文件内容为 Base64 编码的 32 字节随机密钥，权限必须为 0600，运行用户须可读。
# 建议绝对路径；与数据库一起备份，更新程序时必须保留原密钥。
CLOUD_MASTER_KEY_FILE='/usr/local/etc/101cloud/master.key'

# 后端允许访问的 Host Agent 节点管理 IP 范围，不是浏览器访问白名单。
# 多个 CIDR 用逗号分隔；IPv4 单个地址用 /32，IPv6 单个地址用 /128。
# 按实际节点填写；默认仅允许 127.0.0.1/32,::1/128。
CLOUD_NODE_ALLOW_CIDRS='192.168.0.111/32'

# 登录会话有效期，默认 12h；使用正的 Go duration，例如 30m、12h、24h。
# 不支持 1d 写法。登录到期后需重新登录，绑定该登录的控制台连接也会关闭。
CLOUD_SESSION_TTL='12h'

# 每个用户允许同时占用的控制台会话数量，默认 4，范围 1–100。
# 包括准备中、就绪及已连接的终端/VNC 会话，不是实例数量限制。
CLOUD_CONSOLE_MAX_CONCURRENT='4'

# 可信反向代理的来源 IP/CIDR，多个用逗号分隔；默认空，即不信任代理头。
# 仅当请求来自这些地址时，才用 X-Forwarded-For 获取客户端 IP，供限流和记录使用。
# 下例用于 Nginx 与后端同机；远程代理应填写实际代理 IP，不要填写所有客户端网段。
# 可信地址上的进程可伪造客户端 IP，因此不要设为 0.0.0.0/0 或 ::/0。
CLOUD_TRUSTED_PROXIES='127.0.0.1/32,::1/128'

CLOUD_LISTEN='0.0.0.0:8080'
```

```bash
sudo chown root:root /usr/local/etc/101cloud/backend.env
sudo chmod 600 /usr/local/etc/101cloud/backend.env
```

- `CLOUD_ORIGIN` 是浏览器访问面板的外部 origin，格式 `https://host[:port]` 或 `http://host[:port]`：小写、不带默认端口（443/80）、不带路径、查询、末尾 `/` 或用户信息，必须与浏览器发送的 `Origin` 头逐字相同；不是 Agent 地址。生产必须用 `https://`。
- scheme 决定 Cookie：`https://` 使用 `__Host-101cloud_session`、`__Host-101cloud_csrf` 并带 Secure；`http://` 使用 `101cloud_session`、`101cloud_csrf` 且不带 Secure（浏览器不会在普通 http 下保存 Secure/`__Host-` Cookie）。两者都是 HttpOnly、Path=/、SameSite=Lax、无 Domain。http 下口令和 Cookie 明文传输，启动时会输出警告日志，只用于本地开发。
- 本地前端开发：Vite 开发服务器把 `/api/v1` 代理到 `http://127.0.0.1:8080`（需开启 WebSocket 代理），浏览器访问 `http://localhost:5555`，后端设置 `CLOUD_ORIGIN=http://localhost:5555`。代理不得改写浏览器发送的 `Origin` 头，否则修改请求和 WebSocket 会被 403 拒绝。
- `CLOUD_TRUSTED_PROXIES` 是可信反向代理的 IP 或 CIDR 列表（逗号分隔，IPv4/IPv6），非法条目会使启动失败。留空（默认）表示不信任任何代理，客户端 IP 取 TCP 对端地址。只有 TCP 对端属于该列表时，才采用 `X-Forwarded-For` 中的客户端地址（用于登录按 IP 限速和会话记录）；不读取 `X-Real-IP`。
- `CLOUD_NODE_ALLOW_CIDRS` 是允许接入的节点管理 IP 范围，按实际节点填写，尽量使用单 IP `/32`。
- 使用 `serve -c /path/to/backend.env` 可由后端直接加载文件；也可以继续由 systemd 的 `EnvironmentFile` 注入环境，不要求迁移现有服务配置。
- 文件语法为每行一个 `CLOUD_名称=值`，支持空行、整行 `#` 注释、CRLF、等号两侧空白，以及单引号或双引号包裹的单行值。引号内内容按字面量处理；不执行 Shell、不展开 `$VAR`、不处理转义、`export`、多行值或行尾注释。重复键、非法赋值、NUL 和超过扫描上限（约64 KiB）的行会报错，错误不回显配置值。完整解析成功后才应用文件。
- `-c` 的相对路径和文件内 `CLOUD_MASTER_KEY_FILE` 的相对路径都以进程工作目录为基准；建议使用绝对路径。配置文件包含敏感凭据，建议权限 `0600`，由实际运行用户拥有；主密钥文件仍强制要求 `0600`。
- 不需要配置后端 SSL 证书。生产 HTTPS 由反向代理终结，`CLOUD_ORIGIN` 填浏览器看到的 `https://` 地址。
- 程序自动配置 MySQL 连接的 UTC+8、时间解析和严格 SQL 模式。

## 7. 自动初始化数据库及首个面板管理员

先完成主密钥和backend.env配置，再在管理员Bash终端执行：

```bash
sudo -i
set -a
. /usr/local/etc/101cloud/backend.env
set +a

read -rsp 'MySQL管理员密码: ' MYSQL_ADMIN_PASSWORD; echo
CLOUD_MYSQL_ADMIN_DSN="root:${MYSQL_ADMIN_PASSWORD}@tcp(127.0.0.1:3306)/" \
  /usr/local/bin/101cloud-backend install
unset MYSQL_ADMIN_PASSWORD
```

这一步自动建库、建表和创建业务MySQL用户；backend.env里的CLOUD_MYSQL_DSN仍是业务账号，不要改成root连接。MySQL root若使用auth_socket认证，可将管理员与业务DSN都配置为同一个Unix socket端点，并在root终端执行。

本机TCP默认创建 `'业务用户名'@'127.0.0.1'`，Unix socket默认localhost。远程MySQL必须另外指定CLOUD_MYSQL_APP_HOST，值是**后端访问MySQL时的客户端来源主机/IP**，不是数据库服务器地址；避免随意使用%。管理员与业务DSN必须使用相同网络类型和服务器端点。

以后升级：备份并停止后端后，用受控管理员/迁移账户临时设置 `CLOUD_MYSQL_ADMIN_DSN` 执行 `migrate`，业务 `CLOUD_MYSQL_DSN` 保持不变，不再创建账号。例如：

```bash
read -rsp 'MySQL管理员密码: ' MYSQL_ADMIN_PASSWORD; echo
CLOUD_MYSQL_ADMIN_DSN="root:${MYSQL_ADMIN_PASSWORD}@tcp(127.0.0.1:3306)/" \
  /usr/local/bin/101cloud-backend migrate -c /usr/local/etc/101cloud/backend.env
unset MYSQL_ADMIN_PASSWORD
```

`CLOUD_MYSQL_ADMIN_DSN` 也可放在仅迁移时使用的受限权限配置文件中，通过 `migrate -c` 读取；不要将管理员凭据写入日常服务配置或命令行参数。迁移目标始终为业务 DSN 的显式库名；管理员 DSN 可省略库名，若填写则必须与业务库完全相同。两者经 DSN 解析后的网络类型和端点必须一致（与 install 相同；不会将 `localhost` 与 `127.0.0.1` 等别名视为相同）。错库或端点不一致在连接前拒绝。未设置管理员 DSN 或其值为空时仍沿用业务 DSN，环境变量优先于 `-c` 文件；`serve` 始终忽略管理员 DSN。不应给日常业务用户加建表或授权权限。DDL会隐式提交，失败时先检查实际完成的语句，重复 migrate 会跳过已完成迁移。

不再手工创建面板管理员，也不需要配置管理员用户名或密码环境变量。首次运行 `serve`（或启动下面的systemd服务）时，程序在事务中自动创建随机管理员账号及高强度随机密码，提交后输出一条凭据日志：

```text
WARN initial administrator created ... username=admin-<随机值> password=<随机密码>
```

使用systemd时查看：

```bash
sudo journalctl -u 101cloud-backend --no-pager
```

保存首次凭据并在登录后修改密码。数据库只保存Argon2id密码哈希，审计中不保存明文；**启动日志按你的配置包含明文初始密码，必须限制journal、容器日志及日志采集平台的访问**。后续重启不重置密码或再次输出凭据；丢失首次日志不能通过重启找回密码。已有管理员的数据库升级后不会新建另一名管理员。

初始化状态持久化并以数据库行锁串行，并发首启只会有一个进程创建和输出。该凭据日志不受数据库事务原子保护，若恰在提交后、日志写出前崩溃，需由受信DBA走人工账号恢复，不应清空初始化标记以制造新的管理员。

## 8. 配置 systemd

创建 `/etc/systemd/system/101cloud-backend.service`：

```ini
[Unit]
Description=101cloud Backend
After=network.target

[Service]
Type=simple
User=101cloud
Group=101cloud
WorkingDirectory=/usr/local/etc/101cloud
EnvironmentFile=/usr/local/etc/101cloud/backend.env
ExecStart=/usr/local/bin/101cloud-backend serve
Restart=on-failure
RestartSec=3
TimeoutStopSec=30
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now 101cloud-backend
sudo systemctl status 101cloud-backend
sudo journalctl -u 101cloud-backend -f
```

服务不自动执行数据库迁移或导入节点；仅首次启动自动创建管理员并输出一次凭据日志。数据库不在本机时，服务启动依然要求数据库已可连接。

## 9. 配置 Nginx 反向代理

可直接使用的站点配置：[deploy/101cloud.conf](deploy/101cloud.conf)，按当前 `192.168.0.111` 与 `/etc/101cloud/tls.crt`、`/etc/101cloud/tls.key` 生成。放入 `/etc/nginx/conf.d/101cloud.conf`，替换旧站点而不是并存；须先安装包含内嵌前端的后端二进制，再执行 `nginx -t` 和 reload。更换域名时同步修改 `server_name`、证书及后端 `CLOUD_ORIGIN`。

以下片段中 `map` 放进 Nginx 的 http 级配置，`location` 放进已有 HTTPS server；域名和证书由入口服务管理。移除旧静态 `root`/`try_files` 及旧静态目录匹配的 location，把所有路径交给同一个后端。示例与上文 `CLOUD_TRUSTED_PROXIES='127.0.0.1/32,::1/128'` 配套（Nginx 与后端同机，经回环地址转发）：

```nginx
map $http_upgrade $cloud101_connection_upgrade {
    default upgrade;
    '' close;
}

location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $http_host;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $cloud101_connection_upgrade;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;
    proxy_buffering off;
    proxy_cache off;
}
```

```bash
sudo nginx -t
sudo systemctl reload nginx
```

前端与 API 应使用同一个外部 origin。不要把内部 8080 端口无保护暴露到公网；只开放反代入口。

`X-Forwarded-For` 使用 `$remote_addr` 覆盖而不是 `$proxy_add_x_forwarded_for` 追加，这样客户端自带的该头不会传到后端。注意：信任某个地址段意味着该段内任何连接都能伪造客户端 IP。信任 `127.0.0.1/32,::1/128` 时，本机任意进程以及经 SSH 隧道（`ssh -L`）从回环地址连入 8080 的用户都能通过伪造 `X-Forwarded-For` 绕过按 IP 的登录限速（按账号的限速仍然有效）。只列出实际的代理地址；不需要时保持为空。

当前控制台上游预连接属于准备该会话的进程，推荐先单后端进程部署。多进程或多副本需要会话亲和，不能让控制台创建和 WebSocket 连接随机落到不同进程。

控制台没有固定时长上限，也不通过自动续期延长会话；`CLOUD_SESSION_TTL` 默认12h的登录有效期仍然适用。每2秒核对绑定登录、用户启用状态、实例权限和运行状态；注销、登录到期、用户禁用、权限撤销或实例停止会关闭连接。默认每用户最多4个 preparing/ready/active 会话，帧/输入限制、准备超时及网络心跳继续生效；网络断开或服务停止同样会关闭连接，不保证永久在线。

正常停服只关闭本进程持有的预连接并记录 closed。异常退出可能留下 ready/active 行；缺少持久化进程归属信息时不能把本进程看不到的连接当作其他进程已失效。此类记录仍占并发名额，可由原登录通过 DELETE 会话接口关闭；绑定登录到期/撤销或用户禁用后后台也会关闭记录。不要按控制台年龄清理；多副本场景不得无差别关闭其他进程的会话。

## 10. 检查是否启动成功

服务器本机：

```bash
curl -f http://127.0.0.1:8080/api/v1/health/live
curl -f http://127.0.0.1:8080/api/v1/health/ready
```

外部入口：

```bash
curl -f https://cloud.example.com/api/v1/health/ready
```

就绪接口成功不代表已完成业务配置，也不代表 GPU、KVM 或节点网络已验收。新构建的后端同时提供内嵌管理面板与 API，浏览器打开同源根路径即可；接口调用和前端开发参见 [接口文档](后端接口文档.md) 与 [OpenAPI](openapi.json)。

## 11. 首次业务配置顺序

管理员登录需要先获取 `/api/v1/auth/csrf`，保存 Cookie，再提交登录请求。修改操作还要按接口文档携带 CSRF、幂等键及 If-Match。

建议顺序：

1. 创建可复用的价格、GPU价格、额度、GPU空闲、GPU调度、监控政策配置档，直接编辑或无引用时删除；没有平台默认绑定。
2. 导入Agent endpoint、Token和证书指纹，显式选择节点metrics_policy_id（metrics通用ID），探测后启用节点。
3. 在产品目录登记镜像和初始化模板（如需）。镜像按节点/项目配置架构、完整指纹、cloud-init和GPU兼容性，支持批量节点：local只验证已有本地镜像，remote通过所选节点发现真实官方目录并异步拉取。跟踪返回的逐节点image.prepare任务，只有ready配置才能用于该节点创建/导入；登记成功不代表下载完成。
4. GPU只配置共享名额和可用状态，不绑定价格；共享名额不保证显存隔离或算力。GPU价格由实例冻结的套餐gpu-pricing绑定决定，同型号可用不同套餐价格，历史分配不重定价。
5. 登记局域网 IP 池和地址（池名称全局唯一），绑定现有 bridge 或适用的 macvlan，验证后启用。没有 NAT 或端口映射。实例所在节点由所选网络绑定决定，直接下发自定义实例时须指定 node_id。

镜像源管理接口为 `GET /api/v1/admin/image-sources`、`GET /api/v1/admin/nodes/{id}/remote-images?source_id=images`；仅固定可信HTTPS源images（Ubuntu OS同样从该真实目录选择，不支持Canonical Cloud专用格式）。`POST /api/v1/admin/images` 接收 `{code?,version?,instance_type,source_type,properties,nodes:[{node_id,project_name,architecture,fingerprint,cloud_init,gpu_ready,source_id?,variant?,properties?}]}`，返回201 Image及`nodes[].task_id`。查看 `GET /api/v1/admin/images/{id}/nodes`，用带If-Match的 `POST /api/v1/admin/images/{id}/nodes` 批量管理配置，后者返回202及`{image_id,tasks}`。选择实例镜像时传 `GET /api/v1/catalog/images?node_id=...&project_name=...`，只列该节点/项目ready配置；project_name可选但须同时指定node_id，省略则为节点默认项目。GPU-ready为人工兼容性断言，不安装驱动。

`20261102_00_node_images.sql` 在95迁移之后建立 `image_nodes`，历史 `init.sql` 保持原始checksum。现有local镜像与节点按default_project_name交叉生成pending配置和verification-only任务，未核验前绝不ready；旧remote记录不推断可信源。迁移不改现有实例冻结指纹/架构、额度或原任务（含unknown任务）。迁移任务参数仅包含字符串image_node_id；新API排队任务同时包含入队时冻结的config（image_id/node_id/project_name/architecture/fingerprint/instance_type/source_type/source_id/cloud_init/gpu_ready/variant），worker操作原生API前还会固化不可变日志。旧local镜像properties.cloud_init显式布尔值保留；仅当该属性缺失时，worker根据原生镜像properties.variant是否为cloud推断cloud-init，GPU兼容性绝不自动推断。派发结果不明须对账，不盲目重复下载；失败不删除已有原生镜像。

6. 显式选择套餐node_ids，配置这些节点内网络和节点/项目镜像，设置GPU初始策略、用户动作与权限后启用。多个套餐可同时启用，任意时候可编辑或独立停用；active保存完整验证就绪性，既有实例冻结配置不变。管理员创建用户后需明确配额（初始全部为0）；自助注册用户使用注册设置中的默认配额，自定义实例还需max_resident_instances。VM仅CPU；可选GPU容器提供型号及正数gpu_count范围并绑定gpu-pricing。用户可count=0，但价格上限仍受套餐约束。
套餐及custom创建/导入必须提供policy_versions：pricing/credit/metrics始终必填，容器另需gpu-idle/gpu-scheduling，支持GPU的容器套餐（含可选GPU）及启用GPU的custom容器另需gpu-pricing。均为通用政策ID；实例准入冻结绑定，后续启动、GPU操作、空闲/均衡、监控及计费不读取当前套餐或全局默认。

GPU价格配置档候选仅通过管理员`GET /admin/catalog/gpu-options`供custom下发、导入及实例GPU管理显式选择；不提供普通用户全局GPU价格目录，用户报价和GPU价格来自所选套餐及实例冻结绑定。部署历史中旧目录路径仅描述当时版本。

迁移遗留custom实例若尚无gpu-pricing绑定且从未有过任何GPU分配，管理员须在首次挂载前通过`PUT /admin/instances/{id}/gpu-pricing-policy {policy_version_id}`显式选择active且已生效的GPU价格配置档；要求CSRF、Idempotency-Key及If-Match，返回policy_versions、revision和新ETag。此接口仅补齐缺失绑定，已有绑定、elastic或任何历史分配均409 policy_binding_frozen；不替换冻结价格、不重定价历史。

POST /admin/users必填credit_policy_version_id，选择用户独立共享钱包政策。bootstrap管理员可无钱包；POST /admin/credit-accounts {user_id,policy_version_id}遵守标准幂等契约，为未配置用户创建钱包，不立即发放额度。后台只处理已配置账户，游标尊重实际创建时间。实例credit绑定管理各自宽限/停机/存储，选择套餐不覆盖钱包、不按实例增发。混合套餐账户返回enforcements数组，历史事件期限保留。

7. 先受控验证一个实例的创建、登录、停止、磁盘保留、GPU观测和控制台，再开放给成员。

客机 SSH 初始化脚本见项目根目录 [`init-openssh.sh`](../init-openssh.sh)。支持 Ubuntu/Debian（`openssh-server`）和 Arch Linux（`openssh`），以 root 执行；管理员可将脚本登记为 `cloud_init` 阶段模板并绑定套餐，先等待 cloud-init 再安装和配置 SSH。这是独立、显式的管理员脚本阶段，不属于平台基础 cloud-init 网络和 root 密码配置。后端不自动安装 OpenSSH、不自动登记/绑定模板、不改防火墙。脚本不设置 root 密码；新建实例不再写 SSH 登录模式文件，脚本按已有默认密码模式处理。历史模式文件和已发布模板保持原样。默认确保 IPv4 TCP 22 监听；原有 Allow/Deny、来源 Match 和 PAM 策略保留，仍需在目标镜像验收登录。不要把明文密码写进模板或初始化参数。

脚本支持 systemd，以及 Debian/Ubuntu 的 SysV；Ubuntu 存在 `ssh.socket` 时切换为 SSH 服务直接监听。改动前备份到 `/etc/ssh/101cloud-backup.*/sshd_config`，用 `sshd -t` 和 root 上下文的 `sshd -T` 检查配置；生效后失败会尝试恢复配置并重启服务，但不会卸载软件包或还原所有服务启用状态。重复执行不会叠加平台配置块。需能访问软件源；Arch 使用现有同步数据库执行 `pacman -S --needed`，不单独刷新数据库或隐式全系统升级。

Fedora 使用 `dnf install -y openssh-server` 和 systemd 的 `sshd.service`；需要提供 dnf、可用软件源及正在运行的 systemd，不支持通过该路径配置 rpm-ostree 原子系统。脚本不修改防火墙或 SELinux 策略。

### 节点库存自动采集

后端执行 `serve` 时自动启动 `node-inventory` 后台轮询，启动后立即采集，之后每 20 秒一轮；不需要新增环境变量、前端自动刷新开关或独立定时任务。手动运行与 systemd 运行行为一致。后端主动通过固定证书 HTTPS 读取 Agent 的 `/agent/v1/health` 和 `/agent/v1/resources`，不是 Agent 向面板主动推送。

- 覆盖所有已登记节点，包括空节点、无 GPU 节点，以及维护、排空和禁用节点；不依赖已有实例、实例监控采集间隔或 GPU 自动策略。只刷新观测，不自动启用节点、不调整容量、不创建或启停实例。
- 单轮最多并发 8 个节点，每节点 Agent 请求共用 10 秒超时。成功更新硬件/GPU 库存、库存时间和心跳；新发现的 GPU 仍默认禁用，不能自动分配。故障节点较多时整轮可能超过 20 秒，超时并不代表采集成功。
- 健康检查失败标记离线，不推进心跳或库存时间；健康检查成功但资源请求失败或响应不完整，不推进库存时间。恢复后后续轮询自动重新采集。页面与准入的库存过期保护保留，不以刷新时间掩盖采集失败。
- 保存前锁定节点并检查连接凭据与观测时间，连接已更换或已有更新观测时丢弃迟到结果。纯库存刷新不改变节点编辑 ETag，健康状态变化仍增加 revision。GPU 自动调度采样只维护调度指标，不再覆盖节点库存与心跳。
- 库存有效期由平台统一固定为 60 秒（包含边界）；报价、创建准入、resize 和 GPU 分配的节点库存与硬件 `last_seen_at` 使用同一规则，缺失、未来时间或超过 60 秒均不可用，不再引用 GPU 调度的 `metrics_max_age_seconds`。报价只判断硬件/名额，不把动态负载缺失伪装成库存不足；最终分配仍会重新检查负载。
- 独立 `node-gpu-load` 轮询向 `node_gpu_load_snapshots` 保存负载与单独的 `sampled_at`，不更新库存、心跳或硬件 `last_seen_at`；库存轮询也不更新负载时间。为支持尚无实例的首次放置，它按已经生效的已发布 GPU 调度策略中最短的正 `poll_interval_seconds` 采集有 GPU 的启用节点；退休策略仍可能被既有实例绑定，故参与此采样间隔。全局取最短值是保守策略，会让较慢策略的节点也承担额外采集；还会与自动均衡自身的阈值观测分别请求资源，应据节点数量和 Agent 成本合理设置间隔。动态负载请求在数据库事务/行锁之前完成，时间取请求开始而非结束，整帧原子替换且不合并旧卡指标；保存时核对节点连接及时间顺序，替换连接清除旧负载。
- 非固定 UUID 的初次分配、附加、重新绑定统一读取这份独立负载，按实例绑定策略的 `metrics_max_age_seconds` 检查，不接受缺失、未来、过期或不完整指标，不能拿新库存续期旧负载。管理员固定 UUID 不依赖负载评分，但仍检查 60 秒硬件库存、容量/占用和价格；卸载及归属释放仍使用原有完整实例身份、进程、设备引用及显存证据，绝不使用库存或主机负载代替安全证明。
- 新提交/编辑的 GPU 调度策略必须满足 `poll_interval_seconds < metrics_max_age_seconds`；相等也拒绝。历史策略记录不改写，仍按既有绑定读取；历史不满足此关系的配置再次提交时会被拒绝，需显式调整。
- 页面“刷新”读取已保存的观测；“探测库存”仍可立即请求完整探测，并额外核对 Incus 项目及未纳管实例。自动轮询不代替这项完整盘点。

本次完整拆分包含前向迁移 `20261107_10_node_gpu_load.sql`；升级应在备份、停服后执行迁移并替换构建产物，不修改已应用的历史 SQL。旧库存负载不会回填为新动态样本；迁移后非固定 UUID 分配需等待独立采样成功，避免把旧库存的时间当作新负载时间。无需 Agent 协议变更，尚未部署到服务器；本地隔离 TLS Agent 夹具不代表真实 Incus/GPU 硬件验收。

本地验证（2026-11-07，未部署）：独立初始化 MySQL 8.4.13 临时实例，所有新消费者和固定证书 TLS Agent 回归均实际连接数据库；覆盖短指标时效下 30 秒硬件可用、长指标时效不能放行 61 秒硬件、缺失/未来样本拒绝、旧库存负载不能兜底、固定 UUID 与释放证据边界、连接替换及锁外网络。真实注册 Worker/HTTP 烟测记录库存 `09:10:37.192343 → 09:10:57.195476`（约 20 秒），独立负载时间 `09:10:56.196697`；另运行原生后端二进制完成 migrate/serve、CSRF/管理员登录、就绪与资源 HTTP 200，零实例节点成功采集，Agent 503 时库存/负载均不续期并在恢复后重新在线。后端 race 回归及 vet、前端 57 文件/586 用例与 vue-tsc、嵌入前端的构建已通过；前端构建仍输出现有目标环境 BigInt/top-level-await tolerated-transform 提示，未将本地 TLS 夹具称为真实 GPU 交付验收。


### 实例首次登录凭据（仅创建时）

最终 `POST /api/v1/instances` 和管理员 `POST /api/v1/admin/custom-instances` 的顶层只写 `login` 二选一：`{root_password:string}` 或 `{ssh_public_keys:string[]}`。两者都非空或都为空返回 422；省略 login 仍支持无凭据创建。密码为 8–128 个 Unicode 字符、禁止控制字符、保留空格。公钥最多 10 条（去重前）、每条最多 8192 UTF-8 字节；接受 Ed25519、RSA >=2048 位、ECDSA P-256/384/521，拒绝私钥、证书、选项、多行和畸形键，去注释并规范化去重。报价、导入及 `approved_config` 拒绝 login；凭据不属于 `Spec`、`init_parameters` 或模板参数；新建摘要 none/password/ssh_key。

镜像须在所选节点/项目标记 ready、cloud_init=true，否则 409 image_login_unsupported；VM 需可用 incus-agent。密码先生成随机盐 SHA-512 crypt，或规范公钥，均仅存既有 MasterKey AES-GCM login_config_ciphertext，无新迁移。已完成实例不重置；历史待创建混合凭据按原密文应用，不允许新建混合请求。

所有新建 cloud-init-capable 实例保留 `ssh_deletekeys:false`、`ssh_keys:{}`、`ssh.emit_keys_to_console:false`、`no_ssh_fingerprints:true`、`allow_public_ssh_keys:false`、`ssh_publish_hostkeys.enabled:false`，不生成/发布 host keys。禁止 schema 非法的 `ssh_genkeytypes:[]`。密码模式设置 root 哈希、lock_passwd:false、chpasswd.expire:false；公钥模式设置 root ssh_authorized_keys、hashed_passwd:"*"、lock_passwd:false、disable_root:false，密码不可用且不锁住公钥账户。不设置 ssh_pwauth、模式文件或安装/启停 SSH；网络与管理员脚本不变。

新建且冻结 `creation.cloud_init_capable:true` 的容器，在首次 `state.start` 意图之前持久记录 `create.cloud-init-prepare` 并调用节点 `POST /agent/v1/instances/cloud-init/prepare`。请求显式绑定项目、名称、UUID、平台实例归属及完整基础镜像指纹；后端和 Agent 均核对停止状态与身份。Agent 检查固定 `/usr/bin/cloud-init`、`/etc/cloud/cloud.cfg`，确认安装后安全准备固定 `/etc/cloud/cloud.cfg.d/99-101cloud-preserve-hostname.cfg`（内容仅 `preserve_hostname: true\n`、root:root/0644，父目录必须安全，已有冲突文件拒绝覆盖），随后仅删除固定 `/etc/cloud/cloud-init.disabled`（不存在即成功）；只有类型化 Incus404表示缺失，权限/传输/其他错误均阻止首次启动。新建 user-data 同样保留 `preserve_hostname:true`，但系统级固定配置用于覆盖早于 user-data 合并的 local 阶段。同步准备操作可在尚无首次启动意图时幂等恢复，仍通过任务租约派发栅栏保护取消/清理；一旦启动可能已派发，绝不重新准备。现存/纳管实例、后续启动及重启、源镜像均不改动。停止状态文件访问不适用于 VM，因此此步骤明确仅适用于容器，VM 沿用原有首次启动流程。先部署支持该端点的 Agent，再发布后端；不支持端点的节点失败关闭，不回退到脚本或修改镜像。无需数据库迁移。

固定只读等待要求 cloud-init status --wait 成功且 boot-finished 存在。密码模式比较 guest 缓存配置和 shadow 并拒绝锁定/强制过期；公钥模式要求 shadow 为 "*" 且 root authorized_keys 包含所有请求的无选项 type/blob。检查不输出凭据、不调用外部密钥工具；失败沿用 initialization_password_not_ready/root_password_not_ready 安全凭据分类。成功才写 login_initialized_at，错误/降级/超时仍阻止 ready，后续启动不重跑初始化。

公开错误为 `initialization_cloud_init_degraded/cloud_init_failed/tool_missing/marker_missing/password_not_ready/script_failed/timeout/result_unknown/diagnostic_unavailable`（均带 initialization_ 前缀）。details 仅含固定安全分类、建议与可选原始 exit_code；root_password_not_ready 表示所选登录凭据未通过只读验证，completion_marker_missing 仅指 boot-finished。原 unknown、安全回收及禁止重放策略不变，历史 operation 不按新协议重解码。


凭据明文、密码哈希和解密 user-data 不进入任务参数/结果、审计、幂等明文、普通 DTO 或 incus_config_snapshot；原生观测和 resize 的 writable 配置只在内存保留 user-data，以免 PUT 删除已有首次启动配置。幂等层只存 MasterKey-HMAC 请求摘要和不含秘密的响应：结果不确定时必须保留完全相同的原始 body/key 重试，不可换 key 重建实例。前端敏感输入仅保存在内存，受理/注销/换账号清空，报价和持久化草稿中不得包含凭据。

**主机留存风险：** 生成的密码哈希仍是敏感凭据，Incus 实例配置以及 guest `/var/lib/cloud/instance/user-data.txt`、`cloud-config.txt`、seed/cache、`/etc/shadow` 和主机/实例备份可能留存它。后端数据库加密不消除这些副本，节点管理员和 guest root 仍可访问；限制 Incus/guest/备份权限，使用高强度唯一密码，优先公钥。平台不自动删除 cloud-init 缓存或执行 `cloud-init clean`，后者会改变首次启动语义。参考官方 [cloud-init 模块](https://docs.cloud-init.io/en/latest/reference/modules.html) 与 [Incus cloud-init](https://linuxcontainers.org/incus/docs/main/cloud-init/)。

每日额度由后台任务处理，首次发放完成前账户可能尚未就绪。自定义实例的申请和沟通在平台外完成，平台只保留管理员 `POST /admin/custom-instances` 直接下发（请求字段仍为 `approved_config`）及已有实例纳管，不提供申请/审批页面或接口。自定义实例按admin_exempt记录，不受每日额度停机影响，仍受资源及max_resident_instances配额约束。当前OpenAPI包含154个操作。

管理员自定义实例登录接入（本地实现，未部署）：复用成员创建的 `normalizeLogin`、精确节点/项目镜像能力检查、加密准入、网络/登录 cloud-config、首次启动前 prepare 和只读 readiness。可选初始化模板仍是独立阶段，`cloud_init` 模板不覆盖登录或网络 user-data；保留原脚本及阶段语义。`admin_exempt`、用户权限、资源/常驻配额与政策冻结不变。已有实例导入不接受 login，也不下发或重置登录凭据。结果不确定时使用完全相同的 body/key 重试；同 key 不同 body 返回冲突。

2026-11-07 本地验收（未部署）：修复前真实管理员handler对顶层login返回422；修复后隔离MySQL下实例模块`go test -race -p 1 ./internal/instances -count=1`及`go vet ./...`通过。真实Router、管理员登录和生产前端浏览器分别提交密码/公钥，均202且仅密文落库，任务/审计/幂等记录无凭据；自定义免额度和所有者保持不变。TLS协议回归覆盖共用cloud-config网络/凭据生成、首次prepare防重、readiness持久完成、两种模板阶段及无凭据旧请求。协议夹具不等同于真实宿主启动验收，未操作线上实例；无新增迁移。

额度耗尽按实例冻结credit政策期限强停而不删盘，可能丢失未保存数据。停止不等于释放GPU；保留分配继续按历史持有区间记录，释放须显式移除。

## 12. 更新、备份与排错

更新顺序：备份数据库和主密钥 → 停止后端 → 上传新二进制 → 用迁移账户执行 migrate → 启动服务 → 检查健康与日志。不要在不兼容的旧程序继续写库时升级结构。

实例列表和详情（普通用户/管理员）新增字符串字段 `image_os_name`、`image_os_release`，通过来源镜像只读关联查询返回真实 OS，包含 disabled/retired 镜像；缺失元数据为空字符串，客户端未知 OS 使用 Linux 图标，不从实例名称推断。此变更不需要数据库迁移，不改现有镜像内容、实例冻结配置、权限或 revision。

自定义申请退役升级需在停服及备份后执行 `CLOUD_MYSQL_ADMIN_DSN='<迁移账户DSN>' /usr/local/bin/101cloud-backend migrate -c /usr/local/etc/101cloud/backend.env`，保留配置中的业务 DSN。新增 `20261030_80_retire_custom_requests.sql` 删除custom_instance_requests、residency_requests和instances.custom_reason；不会删除实例、任务、用户或通用审计。首次install同样按顺序执行旧建表及前向删除迁移；不得修改已应用的init.sql或历史迁移。

移除控制台固定时限的升级迁移为 `20261031_90_console_no_fixed_expiry.sql`，仅删除 console_sessions.expires_at 及两个过期索引，新增 user_id/status 索引；既有 closed/failed/expired 历史状态不变，不重新打开过期会话。升级前停止全部后端进程；确认旧连接均已终止后，可在迁移窗口将遗留 preparing/ready/active 行更新为 closed、写入 closed_at、清空 secrets_ciphertext 并递增 revision，避免遗留记录占用并发名额。迁移本身不盲目关闭可能仍由其他进程持有的会话；旧迁移保持不变。

至少备份：数据库、master.key、backend.env、外部 HTTPS 入口配置；节点导出的原始连接文件也按管理员凭据保护。恢复时数据库与主密钥必须匹配。

| 问题 | 优先检查 |
|---|---|
| 服务启动即退出 | journalctl；数据库连接、主密钥权限、迁移是否完成 |
| 浏览器登录后仍401 | `CLOUD_ORIGIN` 是否与浏览器地址栏 origin 完全一致（scheme、主机、端口）、Cookie是否被保存；https origin 必须真的经 HTTPS 访问，不能用 http 页面配 https origin |
| 登录返回401 invalid_credentials | 用户名或密码错误，或账号已停用；与会话失效的 unauthenticated 不同 |
| 修改请求403 | Origin、CSRF；上下文到期重新获取/auth/csrf |
| 操作412/428 | 刷新详情获取ETag，再按接口提交If-Match |
| 创建409 | 额度、显式配额、节点容量、GPU名额、IP和政策版本 |
| 节点无法接入 | CIDR允许范围、网络可达、Token和固定指纹；不跳过证书校验 |
| 库存持续过期 | 确认运行包含独立 `node-inventory` 轮询的新版后端；检查后端采集日志、Agent/Incus 健康、网络/CIDR/Token/证书指纹。页面刷新不会替代采集，不能靠放宽过期阈值恢复准入 |
| 任务unknown | 查询原任务并让管理员核对，不重复创建或直接释放预留资源 |
| 删除任务unknown / gpu_reclamation_unverified | 原生删除已确认但 GPU 证据不足；检查 Agent 新字段、采集完整性及节点时间，等待删除后新样本再管理员对账，不重复 DELETE 或手工释放 IP/GPU |
| 控制台无法连接 | WebSocket反代、会话ready状态、Origin及进程亲和性 |

GPU 实例删除先确认原生消失，再最多等待约 95 秒的独立监控证据：`collection_started_at` 必须晚于删除确认，`collected_at` 新鲜度不超过 30 秒，库存完整、实例数组显式存在、目标 UUID/项目+名称全部缺席，且顶层 `host_quality.attribution_complete` 必须显式为 true。该 host_quality 保留节点归属完整性，不因其他停止实例的逐实例未知而误判，也不把未知上下文或设备引用当安全。旧 Agent 缺字段时保留全部预留；先升级 Agent，再对原任务核对。没有活动 GPU 分配不等待指标。通过后 IP/GPU、用量与配额终态原子提交；没有持久删除意图不能仅凭消失释放。详见接口文档第 9 节。

删除回归沿用固定证书 TLS 节点 fixture 与连接内 MySQL 临时表，需 `CLOUD_TEST_MYSQL_DSN` 指向可创建临时表的测试 MySQL（按既有约定使用已迁移测试库）；无该变量时 MySQL 用例跳过。用例覆盖删除前缓存到删除后新样本、过期 operation 404/无 ID 意图的对账不重放、重复处理、证据不足保留全部资源及无 GPU 跳过指标。正向等待约 1 秒/场景，负向用调用方 300ms 上下文测试取消，不等待生产 95 秒上限；不代表真实 GPU 硬件验收。

强制清理回归位于 `internal/instances/stuck_delete_mysql_test.go`，使用 `CLOUD_TEST_MYSQL_ADMIN_DSN` 创建隔离库并应用全部真实迁移/外键，经真实 Gin HTTP 变更中间件验证 queued/running/waiting/unknown 接管、权限/确认/重放，再使用固定证书 TLS 原生 fixture 验证初始缺席、强停删除、GPU 指标不可用仍回收、冲突身份/离线保留资源、严格 HTTP+Incus404、已知原生操作取消与恢复、派发中 CREATE 接管串行化以及撤销 worker 不再派发/改 SQL。`platform/jobs_mysql_test.go` 另覆盖撤销后的 finishTask/Step 拒绝迟到写入。不涉及真实节点/硬件删除。

完整架构、配置边界和本地验收记录见 [后端设计方案](后端设计方案.md)。本地协议验收不替代真实 KVM 启动、多GPU驱动热调整及VGA硬件验收；上线前需在获准的测试节点验证。

## 13. 实际部署记录（192.168.0.111）

已部署二进制/usr/local/bin/101cloud-backend、配置/usr/local/etc/101cloud/backend.env及主密钥，systemd服务101cloud-backend已启用开机启动。使用独立业务库101cloud_backend（57张表）及101cloud_backend@localhost，仅授予该库SELECT/INSERT/UPDATE/DELETE；原101cloud库和历史指标验收库保留。

HTTP监听127.0.0.1:8080，就绪检查、随机管理员登录、真实Agent固定证书探测及节点硬件库存查询已通过。管理员用户名admin-ZpM4Bz_jZ26Hy3O4；密码仅从服务器首次启动journal读取，不写入文档。backend.env为root:root 0600，master.key为101cloud:101cloud 0600。

当前没有配置面板HTTPS反代或前端；CLOUD_ORIGIN暂为https://192.168.0.111，仅用作验收Origin配置，不代表该HTTPS地址已有面板可访问。未修改现有80端口服务、防火墙、网卡、路由、驱动或Agent配置。远程临时上传目录与验收SSH隧道已清理，后端/MySQL/Agent继续运行。

该节点实际枚举到1张RTX 2080 Ti。用户提供保留IP192.168.0.116后，先通过Host Agent在ens18执行ARP探测，返回no_conflict_detected、available=true、无回应；再通过后端登记地址并验证macvlan直连网络（/24，网关192.168.0.2），未复用NAT桥、改动宿主网卡或占用已有112/113地址。

真实验收容器backend-acceptance-116（业务ID1，Incus名cloud-386f33c547cbd58e33b88f12）配置1vCPU、1GiB内存、3GiB磁盘。创建、GPU绑定、WebSocket终端、退出码7、正常停机/启动、启动后116地址及磁盘文件保留、在线空闲GPU移除均已通过。限时真实CUDA负载采到1个归属进程、220MiB显存、近似SM95%，后台已写MySQL；验收时实例样本188条、GPU样本179条、各17条汇总，7个忙碌GPU点，父子关联孤立记录0。数量是验收快照，持续采集后可能增长。

联调修复了两个真实协议问题：CPU时间窗两个端点统一量化为微秒并据此计算独立窗口，不再拒绝Agent的纳秒精度；终端正确响应Incus空文本EOF握手并预先等待真实操作结果，不伪造退出码。修复后的后端已部署，配置、Agent与数据库结构未因此改变。

测试负载已停止，GPU分配确认released，测试容器最终Stopped，磁盘数据和116地址预留保留供后续检查。其他原有实例状态不变。节点管理状态已回到maintenance，GPU在后端标记disabled；当前只有显式的零价格验收政策，未配置成员正式价格、额度或套餐，不应直接开放自助创建。未完成真实多卡均衡（本机只有一张可识别GPU）及KVM/VGA验收。

后续按用户要求直接SSH到服务器、在本机HTTP调用API复测（未做换卡）：登录/CSRF、缺少或陈旧If-Match拒绝、同幂等键返回相同任务及异参409、维护态刷新节点库存、116容器启动、客机exec真实退出状态和文件读取、等待客机网络就绪、单卡在线绑定及nvidia-smi、最新指标入库、空闲在线移除GPU、正常停机均通过。复测快照实例样本556条、GPU样本182条、实例汇总48条，孤立GPU子记录0。测试后保持验收容器Stopped、GPU已释放、节点maintenance及GPU disabled，其他原有实例状态未变。未声称本轮重新执行CUDA负载或多卡均衡。

2026-10-30 契约修正部署：新增 `CLOUD_TRUSTED_PROXIES`（X-Forwarded-For）、`CLOUD_ORIGIN` 支持 http/https（Cookie 名称与 Secure 随协议变化）、密码错误独立返回 `invalid_credentials`、列表与详情字段类型统一、对外政策版本引用统一为 `policy_versions.id`、弹性实例可选择不带 GPU。本地临时 MySQL 8.4 上 `go test -race ./...`（含 MySQL 回归）与 `go vet ./...` 通过，并用真实 HTTP 进程（http Origin、可信代理 127.0.0.1）验证 27 项：HTTP Cookie 名与 Secure、两类 401、改密码失败会话保留、按转发 IP 分桶限流、政策/镜像/地址池/绑定列表字段类型、GPU 定价与额度账户 ID、可选 GPU 套餐发布、报价 0 张/1 张/超范围。服务器仅替换二进制（备份 `/usr/local/bin/101cloud-backend.bak-20261030172121`），`backend.env` 与数据库结构未改；部署后只读核对真实数据：列表字段为对象/数组/布尔，GPU `gpu_price_version_id`、`/catalog/gpu-options` 的 `id`、使用记录与 GPU 分配的版本 ID 均等于对应政策版本 `id`，HTTPS Origin 下仍为 `__Host-` Secure Cookie，错误密码返回 `invalid_credentials`。当前未配置 Nginx，`CLOUD_TRUSTED_PROXIES` 保持为空。

2026-11-02 节点镜像版本已仅部署 `.111`：新增 `20261102_00_node_images.sql`，旧本地镜像通过任务73/74精确原生核验后ready，显式保留旧GPU声明并从真实cloud变体补齐缺失的cloud-init信息。浏览器从节点Agent真实目录登记并下载Alpine3.22/cloud/x86_64（镜像3、任务76），原生完整指纹 `04dbb9362a46ef57716981d224a47577d1dde1ad6c2101d2a517d375e2666992`、19.34 MiB、私有、default项目已核对；旧镜像2另经本地管理任务77只读验证成功。创建选择器按实际网络项目查询就绪镜像，其他项目无配置时为空。实例15/18/19的冻结指纹、架构、规格、状态及未知任务43均保持原状，未触发任何实例生命周期操作或修改真实政策/配额。后端vet、全量race/真实MySQL/TLS回归及前端类型/lint/build通过；多节点差异配置和部分失败在隔离节点协议环境验证，线上仅有一个管理节点。完整数据库、旧后端、前端链接和Agent备份位于 `/var/backups/101cloud/`，分别为 `before-node-images-20261102.sql.gz`、`backend-before-node-images-20261102`、`frontend-before-node-images-20261102.link`、`hostagent-before-node-images-20261102`。前端发布目录与详细验收见 `../frontend/README.md`。

2026-11-02 套餐配置版本仅部署 `.111`：应用 `20261102_10_plan_configuration.sql`，新增节点白名单及套餐 GPU 策略、实例动作、GPU 编辑权限；创建 elastic 实例冻结配置与权限，允许降低但不能突破套餐 GPU 上限，用户生命周期/GPU/终端操作与策略字段均执行授权。节点选择与网络实际节点一致，不变更既有实例位置。`go vet ./...` 和隔离 MySQL 8.4 下 `go test -race -p 1 ./... -count=1` 全量通过，覆盖真实迁移/外键、带固定证书的 TLS 存储准入、HTTP 创建/权限变更、终端新建及既有会话授权撤销、节点范围、空权限和配置快照保留。初次并行回归中新增夹具使用退役配额字段的问题已修正；另一次既有删除证据用例在并发压力下300ms前未到达采集步骤，串行全量验证通过，未修改删除生产逻辑或该用例。

真实后端进程和浏览器在隔离库验证套餐复制、草稿保存、发布与只读回显，未声称真实 GPU 换卡/空闲回收硬件验收。OpenAPI通过规范校验，303个内部引用可解析。上线前完整备份 `/var/backups/101cloud/before-plan-configuration-20261102.sql.gz`（gzip完整性通过），旧后端 `backend-before-plan-configuration-20261102`，旧前端链接 `frontend-before-plan-configuration-20261102`。已重启后端，服务active；8条既有实例的状态、冻结指纹/架构/规格、GPU策略和授权快照逐项核对不变，无实例创建/停止/调整操作，Host Agent与Nginx不改。前端发布及实际界面验证见 `../frontend/README.md`。

2026-11-02 可编辑套餐版本仅部署 `.111`：应用 `20261102_20_editable_plans.sql`，套餐切换为独立active/disabled、默认disabled，启停不影响其他套餐，所有状态均可直接编辑（active编辑仍完整校验就绪引用）。旧draft/retired迁为disabled，active不变。新增实例plan_snapshot冻结资源基准与allowed_overrides；新建匹配报价plan_revision，旧报价409 plan_changed且不产生资源副作用，resize只读冻结范围和实例原价格/政策。`go vet ./...`、隔离MySQL 8.4上的 `go test -race -p 1 ./... -count=1` 全量通过，覆盖多套餐独立启停/编辑、失败回滚与ETag冲突、报价失效、新旧实例规格隔离、停用后按原冻结范围调整、fresh/upgrade迁移。前端类型检查/oxlint/build与OpenAPI规范校验通过，303个内部引用可解析。

真实后端进程与浏览器完成双套餐同时启用及在线编辑、独立停用/重新启用、冻结范围调整弹窗只读验收，测试数据仅存在隔离本地库；不声称真实GPU或虚拟机生命周期硬件操作。线上后端active，已有8条实例和1个active套餐的原状态、规格及策略授权快照一致；该套餐code1/v1的revision仍为2，未在线上修改套餐或实例。备份 `/var/backups/101cloud/before-editable-plans-20261102.sql.gz`（gzip完整性已验证）、旧后端 `backend-before-editable-plans-20261102`、旧前端链接 `frontend-before-editable-plans-20261102`；未改Host Agent、Nginx或长期运行账户配置。

2026-11-02 仅部署 `.111` 报价名称解耦修复。`ValidateElastic` 保留计费配置/节点/镜像/GPU/策略校验，移除与创建准入重复的名称检查；`POST /quotes` 不要求名称，`POST /instances` 仍独立拒绝无名称、空白或超过128字符的名称。无数据库迁移。隔离MySQL上的全量 `go test -race ./...`、`go vet ./...` 与Linux amd64构建通过；新增HTTP回归核对报价价格不受名称影响、拒绝创建不产生实例/任务/计费预留及有效名称创建。旧二进制 `/var/backups/101cloud/backend-before-quote-name-20261102`，前端及浏览器验收见 `../frontend/README.md`。


2026-11-03 仅 `.111` 部署欠费重试写放大修复，无数据库迁移，无前端/Host Agent/Nginx及MySQL长期配置变更。`go vet ./...`、真实隔离MySQL下 `go test -race -p 1 ./... -count=1` 及Linux构建通过；新增回归覆盖无余额重复调用保持账单revision/时间及流水不变、首次欠费状态落库、部分付款后继续充值补扣、过期预留仅允许原会话消费，以及不消费过期非预留/未来额度。线上观察超过129秒，历史明细180541/180542的revision均保持4670、金额与updated_at不变，8个计费会话游标继续推进；全量账单已支付金额与结算流水差异为0。部署前后10条实例的状态、冻结指纹/架构/规格、GPU策略和授权一致，旧后端 `/var/backups/101cloud/backend-before-debt-retry-20261103`。

按用户授权清理binlog前已核对无复制通道、注册副本、binlog读取连接或已发现的本机日志备份计划。分别生成修复前/后的全库一致性备份（single-transaction/source-data=2，所有7个数据库、routines/events/triggers/hex-blob），gzip完整性与SHA256匹配；文件在本机 `/Users/nagisa/developer/101cloud-backups/` 与服务器 `/var/backups/101cloud/` 双份保存且0600：`all-databases-before-billing-binlog-20261103-111631.sql.gz`（binlog.000357:55145951）和 `all-databases-after-billing-fix-20261103-112846.sql.gz`（binlog.000361:158）。使用MySQL `PURGE BINARY LOGS TO 'binlog.000361'`，未手工删除日志、未RESET/关闭binlog，保留新备份坐标起的恢复链路；旧日志不再支持备份前时间点恢复。基线198个旧日志清理后仅余binlog.000361（验收时5.50MiB），可用空间3.18→22.37GiB，使用率97%→76%；MySQL及后端正常。修复后该保留日志中52次计费UPDATE均有业务字段变化，只有时间/版本变化的UPDATE为0。

2026-11-03 仅 `.111` 部署管理员额度发放卡顿及结果说明修复，无数据库迁移。真实记录核实：账户2的额度桶18/19各发放22pt并各被历史欠费完整抵扣（1259/1547条结算流水）；请求从发放到事务提交均约27秒，非过期时间解析问题。修复资金耗尽后继续遍历欠费及逐笔重复读取资金来源/会话的长事务，正数仍优先偿债，不免债、不重复补发。`go vet ./...`、真实MySQL全量 `go test -race -p 1 ./... -count=1`、前端类型/oxlint/build及OpenAPI规范校验通过；回归覆盖有/无/null过期时间、6001笔欠费跨页和半笔付款、欠费尾部不变、补扣后余款、负数扣减保留预留、同键重放不重复发放与资金流水守恒。

真实后端serve和浏览器在隔离库6001笔每笔0.02pt欠费下分别发放22pt：不设过期约1.058秒，设过期约1.061秒，分别显示偿债22pt、可用0、未偿98.02/76.02pt；无欠费账户发放22pt约0.616秒并显示可用22pt。模拟已提交后详情GET503，结果仍保留且提示勿重复提交，数据库仅新增1pt、最终23pt。该性能为本地隔离环境测量，不等同于线上硬件延迟承诺。线上只读打开确认提示后取消，未对真实账户提交测试调整；发布前后11条实例配置/状态、原两笔22pt发放及全部管理员发放总量一致，已支付金额与结算流水差异0、锁等待0。前后端及恢复备份位于 `/var/backups/101cloud/`：`backend-before-credit-adjustment-20261103`、`frontend-before-credit-adjustment-20261103.link`、`all-databases-before-credit-adjustment-20261103-130934.sql.gz`（本机同名副本在 `/Users/nagisa/developer/101cloud-backups/`，gzip/SHA256匹配，恢复坐标binlog.000363:66728416）。前端发布目录 `/srv/101cloud/releases/20261103-credit-adjustment`；未修改Nginx、Host Agent、MySQL配置或原财务数据，未再次清理binlog。

2026-11-03 异步额度修复仅部署 `.111`。前向迁移 `20261103_10_async_credit_settlement.sql` 新增持久化待处理标记与查询索引。完整隔离MySQL `go test -race -p 1 ./... -count=1` 通过，包含6001笔欠费、零同步补扣、有界批次/资金来源、批次间发放、事务回滚、重启恢复、幂等、精确流水、过期与预留归属。前端vue-tsc、变更生产文件oxlint及生产构建通过；全src lint仍有22个既有非空断言违规（主要在instance-order.test.ts），未为本次修复改写无关测试。

真实后端与浏览器在隔离库6001笔、总计120.02pt欠费下发放22pt，POST201约26ms、界面约0.145秒显示已入账，净余额立即-98.02pt，后台实际抵扣22pt。再发50pt的实际HTTP约10.4ms；在pending=true、实际已偿22pt时停止进程并重启，数据库继续处理到已偿72pt、未偿48.02pt、pending=false，逐笔金额与流水差异0。随后模拟发放已提交但详情GET503，入账结果与“勿重复提交”提示保留，手动刷新恢复；独立Chrome检查桌面与390px窄屏。延迟是本地隔离测量，不是线上耗时保证。

线上仅登录、读取就绪接口和真实欠费账户详情，未测试发放或免债。发布前后既有实例配置/状态和全部管理员实际发放金额一致，线上账单已付金额与流水差异0、锁等待0、后端active。发布目录 `/srv/101cloud/releases/20261103-async-credit-settlement`；数据库完整备份及旧前后端保存在 `/var/backups/101cloud/`，全库备份本机副本在工作区外 `101cloud-backups/`，gzip与SHA256一致并保留binlog恢复坐标。未改Host Agent、网络或数据库运行配置。

2026-11-03 16:38:02（+08:00）历史发布仅调整 `.111` 磁盘收费：`100daily` 政策10及对应来源2切换为running_only，其他字段及原free政策不变。账户锁内同步更新公开JSON/计费来源并写审计 `billing.storage_running_only`。已停止实例22/23/25的磁盘会话26/28/32在最后已结算边界16:38:01关闭；检查无held预留后关闭，未删除或改写任何历史账单、流水或磁盘数据，未变更10分钟宽限期。当时后续运行/停止沿用已有running_only生命周期，不需要后端重启；当前全部资源停机免费源码规则见本文开头。

跨11秒（两个5秒结算周期）实际观察：三个已停机磁盘会话的账单笔数、历史应收总额和最新收费时间均不再变化，已停机实例的active磁盘会话为0；运行实例的磁盘会话仍active，后端服务active。变更前完整业务库备份 `/var/backups/101cloud/before-running-only-storage-20261103-163758.sql.gz`，工作区外101cloud-backups保留同名副本。历史欠款保留，未发放或扣减额度。

2026-11-03 套餐唯一配置来源已仅部署 `.111`，前端目录 `/srv/101cloud/releases/20261103-plan-owned-policies`，旧二进制/前端链接及完整恢复备份均保存在 `/var/backups/101cloud/`。应用 `20261103_20_plan_owned_policies.sql`，清除平台默认绑定表、节点GPU定价列，旧明确绑定只在迁移时回填实例/套餐和节点监控策略，运行时无默认回退。用户每日额度独立绑定共享钱包；套餐额度配置仅控制实例计费窗口、宽限、停机与磁盘等规则，不按实例重复发钱。创建用户和缺失钱包初始化须显式选额度档；节点监控策略也显式选择。现有实例绑定及已分配GPU价格保留，不跟随套餐后续编辑；极少数无GPU历史且缺定价的旧custom可经受保护的一次性接口明确设置。

验证：全量真实MySQL8.4 `go test -race -p 1 ./... -count=1`、go vet、前端全量Vitest、vue-tsc及生产构建通过；OpenAPI JSON无重复键且旧默认接口/成员GPU定价目录已移除。隔离真实HTTP路由、SQL准入及只读TLS存储夹具下，同一GPU由两个套餐分别报价4/7pt并返回202，两笔gpu_allocations持久化对应费率且共享同一gpu_id；钱包仍1个、额度桶仍1个、daily_grants为0。低于4pt的用户上限只返回gpu_price_exceeds_limit，不产生伪库存错误。未运行原生交付worker，不将queued验收称为硬件实例交付成功。

真实线上数据副本和正式停服迁移前后均比较全部账单/流水/额度桶金额及GPU分配费率、实例状态/资源，完全一致。切换窗口最新业务库备份 `business-at-plan-owned-cutover.sql.gz` 已gzip校验，全库备份 `all-databases-before-plan-owned-20261103-185805.sql.gz` 本机/服务器SHA256匹配。线上登录只读确认GPU DTO无定价字段、旧默认接口404、套餐2拥有metrics及GPU定价绑定，就绪200、节点在线持续采样、账本差异0、停机磁盘active会话0。线上只请求报价，未创建测试实例或修改资金；共享套餐GPU报价4pt且不再出现定价绑定造成的GPU库存错误。既有套餐1的独占/共享定价冲突保留，需管理员选择兼容配置，不擅改商业价格。

2026-11-04 管理员卡住实例强制清理已仅发布 `.111`，目录 `/srv/101cloud/releases/20261104-admin-stuck-cleanup`，无需数据库迁移。支持管理员DELETE及POST /admin/instances/:id/delete显式cleanup_stuck=true与强制风险/精确名称/删除磁盘确认；非ready、状态过期及已有任务均可提交真实清理任务。锁内取消旧任务并提升租约代数，原生请求派发与操作编号记录有持久化围栏，防止旧worker在接管后继续CREATE/START或改写已删除状态。确认目标缺席后，清理路径不等待独立宿主GPU指标，正常删除原严格回收逻辑不变；关闭计费并一次释放目标IP/GPU/预留，实例保留deleted_at历史记录以维持账单审计引用，从活动列表移除。

验证：真实MySQL全量go test -race -p 1 ./... -count=1及go vet通过；前端49个文件483条Vitest、vue-tsc、变更文件oxlint、生产构建通过。新增回归覆盖接管queued/running/waiting/unknown、成员拒绝、确认/重放、原生运行目标强停删除、初始缺席、UUID冲突、离线保留资源、派发中CREATE围栏、取消响应不等于操作结束、迟到worker不能回写。围栏日志同时写node/project上下文；只读初始化诊断每次单独记录有限长度步骤，不重放原初始化。

实际独立HTTP后端+原生TLS协议夹具+浏览器构造unknown创建任务，管理员DELETE返回202，旧create变cancelled/superseded_by_cleanup，worker确认不存在后delete任务succeeded并跳回实例列表；原生停止/删除请求0次，GPUreleased、IPavailable、active使用会话0、10000pt本金完整及预留0。仅协议夹具，不是线上真实实例删除。线上只打开实例15的清理确认后取消，未删除任何真实实例、未变更原卡住任务或管理员发放；发布前后实例配置/状态、活动任务及发放基线一致，就绪200、账本差异0、停机磁盘active会话0。

备份位于 `/var/backups/101cloud/`：`backend-before-20261104-admin-stuck-cleanup`、`frontend-before-20261104-admin-stuck-cleanup.link`、`all-databases-before-stuck-cleanup-20261104-202151.sql.gz`。全库副本保留在工作区外101cloud-backups，gzip及双端SHA256校验通过。若旧请求完全丢失原生操作编号且仍有提交歧义，清理任务保留unknown/cleanup_native_submission_unknown，需依据可信原生日志恢复操作编号后协调，不把瞬间查无实例冒充不存在晚到CREATE的证明；离线、权限或超时均不判成功。

2026-11-04 仅网络/root密码cloud-init已仅发布 `.111`，目录 `/srv/101cloud/releases/20261104-password-network-cloudinit`，无需数据库迁移。新建请求仅接受login.root_password，删除SSH公钥请求/注入及ssh_pwauth、SSH模式标记；已标记cloud-init能力的镜像即使省略login也下发禁止SSH自动动作的基础配置。使用schema合法ssh_keys:{}选择cc_ssh不生成主机密钥分支（ssh_genkeytypes:[]会被真实Arch schema拒绝），保留原有hostkeys并关闭公钥导入、指纹/控制台输出及发布；不安装、配置或重启SSH服务。网络仍为eth0静态IP/路由/DNS。密码完成检查固定读取客机缓存cloud-config与shadow，在内存比较哈希、拒绝锁定/强制过期，不把凭据放进exec参数或输出，cloud-init非零/超时仍失败。管理员模板独立执行，未改脚本或既有实例密码/初始化checkpoint。

真实验收：隔离Incus项目ci-cloudinit-20261104214610使用本地Archlinux/cloud镜像，profiles为空、1CPU/256MiB/2GiB磁盘、无GPU、p2p网卡及测试网192.0.2.2/24，不使用业务网桥/IP池。首次验证发现空ssh_genkeytypes导致schema降级，修正后用全新容器和实际Go生成配置启动：实际cloud-init及平台完成判定均exit0、done、errors[]、recoverable_errors{}，root shadow哈希匹配且未锁，eth0=192.0.2.2/defaultgw192.0.2.1，ssh-keygen/sshd均不存在、无主机密钥文件及旧模式标记。两只带专用标记的临时容器、项目和远端临时文件已清理，default项目实例/状态与验收前一致。没有重跑既有实例cloud-init。

完整真实MySQL go test -race -p 1 ./... -count=1、go vet、前端50文件483条测试、vue-tsc及生产构建通过。浏览器创建页仅密码/确认，确认页固定掩码；实际报价请求体无login/root_password等凭据，未提交创建。oxlint在errors.ts现有initializationLabel非空断言报1项，未为本次功能改写该无关断言；未宣称该检查通过。线上就绪200，旧ssh_public_keys字段422（未创建实例），实例状态/资源/凭据密文/初始化checkpoint、模板内容、原卡住任务和管理员发放基线前后一致。全库备份all-databases-before-password-network-20261104-220740.sql.gz及旧前后端位于/var/backups/101cloud/，全库本机副本保留工作区外101cloud-backups，gzip和双端SHA256一致。

2026-11-04 已仅发布`.111` `/srv/101cloud/releases/20261104-password-or-public-key`：新建login必须在root_password与ssh_public_keys中选择一种非空方式；两者同时提供或均为空拒绝。公钥经标准密码学解析、类型/长度/数量限制、移除注释及去重后加密保存并写入root authorized_keys。仅公钥模式使用不可用密码`*`且不锁公钥登录，readiness检查实际shadow及所有请求公钥；主机密钥生成仍禁用，不安装/启动SSH，旧实例不改。管理员脚本独立原样保留。

实际Arch/cloud隔离容器验证仅公钥：cloud-init及实际完成判定exit0，done/errors[]/warnings{}；root密码`*`，唯一去重公钥内容匹配，root所有者、目录700/文件600，ssh-keygen/sshd均不存在，无主机密钥。专用临时容器/项目已清理。完整MySQL race回归、go vet、前端50文件491条测试、vue-tsc和生产构建通过；浏览器验证二选一、公钥模式无密码输入和确认项，确认仅显示1条公钥，未提交平台创建。线上就绪200，发布窗口既有实例状态/资源/凭据checkpoint、模板脚本、原任务及管理员发放基线一致。备份all-databases-before-user-public-keys-20261104-223703.sql.gz及旧前后端在/var/backups/101cloud，本机全库副本在工作区外101cloud-backups，gzip及双端SHA256校验通过。

2026-11-05 已仅发布 `.111` `/srv/101cloud/releases/20261105-cloud-init-prepare-020000`，先更新 HostAgent 再更新 backend，无数据库迁移。新建容器首次启动前持久准备步骤及身份校验、幂等恢复、任务派发栅栏已集成；真实新初始化隔离 MySQL 8.4.13 的全量 backend `go test -race -p 1 ./... -count=1` 与 `go vet ./...`、HostAgent 同等 race/vet 检查均通过。两个服务 active，后端 `/api/v1/health/ready` 返回 200。

真实 Fedora 44/cloud 指纹 `014afd33462f7d242c23c63155a90beff801a140bddd3e4d1d6d6f7068b42ffc` 的专用隔离克隆验证：停止状态原有 disabled marker，真实带认证 HTTPS Agent 首次返回 `{checked:true,disabled_marker_removed:true}`，第二次返回 marker_removed=false；首次启动后静态 eth0=192.0.2.2/24、默认网关192.0.2.1、密码哈希或仅公钥（shadow=`*`）分别正确应用。最终项目 `ci-prepare-020000f` 的两只测试克隆在启动前仅从各自克隆删除 ssh-keygen，客机 ssh-keygen/sshd 均不存在且没有生成 host keys，网络及两种凭据仍通过只读验证。**限制：此 Fedora 镜像首次启动另报告 Failed to set the hostname 警告，cloud-init 为 degraded done、严格 readiness exit=2；平台未抑制此错误，因此不能宣称该镜像完整初始化已就绪。** 原 fed35 任务/实例没有重试，源镜像没有改动，所有专用项目/克隆均已清理。

全库（all databases、routines/events/triggers、hex blob）及旧两个二进制/完整配置备份 `/var/backups/101cloud/20261105-cloud-init-prepare-020000/`，本机权限600副本 `/Users/nagisa/developer/101cloud-backups/20261105-cloud-init-prepare-020000/`，gzip 与双端 SHA256 已验证。发布前后25行实例状态/资源/凭据密文哈希/初始化checkpoint、模板内容哈希、活动任务、管理员发放数量/总额、11只原生实例配置/设备/状态以及 Fedora 源镜像元数据基线全部一致；证明保留在备份目录 baseline-before/after/comparison JSON 与发布目录 smoke-results.json。

2026-11-05 hostname 后续修复已仅发布 `.111` `/srv/101cloud/releases/20261105-preserve-hostname-080919`，先更新 Agent、再更新 backend，前端不变、无数据库迁移。实际隔离 Fedora trace 显示 cloud-init local 阶段在 user-data 合并前调用 `hostnamectl set-hostname`，退出1，stderr 为 `Failed to connect to system scope bus via local transport: No such file or directory`；启动后同命令成功。仅 user-data 的 preserve_hostname:true 虽出现在最终合并配置，仍无法阻止该早期警告。因此保留 user-data 设置，并在已有受保护的停止实例准备端点内，于删 marker 前安全写入固定系统级 preserve_hostname 配置，拒绝覆盖冲突内容，不改 systemd/服务或其他镜像配置。

实际 Go `loginUserData` 生成的密码及仅公钥配置，使用原 Fedora44/cloud 指纹在新项目 `ci-hostname-080919-fixed` 的两只一次性容器首次启动验证：真实 Agent 首次准备 marker_removed=true、重复=false；平台原样 `CloudInitReadyCommand` 两种模式均 exit0，cloud-init done/errors[]/recoverable_errors{}，没有 hostname 警告；hostname 保持 Incus 名称，192.0.2.2/24 与默认网关192.0.2.1、密码哈希或 shadow=* 及所请求公钥均通过只读核对。两只测试克隆启动前移除各自 ssh-keygen，客机无 ssh-keygen/sshd、未生成 host keys；原 fed35、源镜像及业务实例未触碰。专用项目/客机和 fixture 临时文件均已清理。

检查：新初始化隔离 MySQL8.4.13 下 backend `go test -race -p 1 ./internal/instances -count=1`（含登录和 worker 回归）通过77.119s，`go vet ./...` 与 Linux/amd64 构建通过；Agent 全量 `go test -race -p 1 ./... -count=1`、`go vet ./...` 与构建通过。两个服务 active，后端 readiness200。完整全库/旧二进制/配置备份及 hostname-diagnostic.txt、smoke-results.json、基线证据保留 `/var/backups/101cloud/20261105-preserve-hostname-080919/` 与本机 `/Users/nagisa/developer/101cloud-backups/20261105-preserve-hostname-080919/`，权限600、gzip/SHA核对。发布前后25行实例状态/资源/凭据checkpoint、模板哈希、活动任务、管理员发放、11只原生实例及源镜像基线全部一致。
