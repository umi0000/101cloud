# 101cloud Frontend

基于 [Vue Vben Admin](https://github.com/vbenjs/vue-vben-admin) v5.7.0（MIT，见 [LICENSE](LICENSE)）的 `web-antd` 应用裁剪而来，只保留 ant-design-vue 版本。设计与后端契约见 [前端设计.md](前端设计.md)。

当前版本开放管理员与普通用户共用登录。管理员进入 `/admin/dashboard`；普通用户进入 `/instances`，侧栏仅“实例管理”，支持卡片首页及创建弹性容器（`/instances/create`，隐藏菜单入口）。后端对 `/api/v1/admin/*` 继续强制 admin 角色。

普通用户头像下拉菜单新增“修改密码”，进入隐藏侧栏的 `/account/password`（未部署）。页面要求当前密码、新密码和确认新密码，新密码按Unicode字符校验12–128字符；仅修改平台登录密码，不修改实例root。首次读取 `/me` ETag，使用CSRF及幂等键调用 `POST /me/password`；当前密码错误保持登录，成功清空账号/任务/订单会话状态并跳登录页。结果未知冻结原body/key/ETag，用户可手动原请求重试或返回登录；412须刷新版本后重新确认。凭据只在组件内存，换号、会话过期或卸载清空，不进入URL、持久存储或Pinia动作。575条测试、类型检查和构建通过；真实隔离MySQL/后端浏览器验收头像入口、错误密码保留会话、成功后会话401、旧密码拒绝与新密码登录成功，375px无横向溢出。

上述修改密码页面已仅前端部署 `.111` `/srv/101cloud/releases/20261105-member-password-page`，旧入口及校验基线保存在 `/var/backups/101cloud/20261105-member-password-page/`。线上静态资源配合隔离只读响应验证头像“修改密码”入口、三个密码字段及完整页面渲染；未向生产提交密码修改。HTML SHA256一致，ready正常；后端、Agent二进制和PID、Nginx配置均未变，无服务重启或迁移。

2026-11-05 已从管理员「政策与定价 → GPU 空闲」、管理员实例 GPU 策略及成员首页进入的实例 GPU 配置中移除闲置利用率/显存百分比配置。实际 GPU 利用率、显存、进程和引用观测继续展示，GPU 调度的源/目标负载阈值保留；空闲回收仍要求零进程/零设备引用/零显存及连续有效观测。前后端已发布 `.111` `/srv/101cloud/releases/20261105-remove-gpu-idle-thresholds`，同时清理后端政策字段及来源列，不再接收或返回两项配置。

政策表单9项测试、类型检查及构建通过；线上静态资源配合隔离 API 验证管理员新建/编辑七项配置、管理员安全规则、成员首页进入 GPU 页后的安全规则与活动指标，以及390px无横向溢出，运行错误/意外API请求均为0。原有成员GPU组件9处非空断言仍使oxlint失败，本次未修改这些行。未对生产实例提交启停/删除/GPU操作；全库、旧程序、配置、前端入口和截图/请求证据保存在 `/var/backups/101cloud/20261105-remove-gpu-idle-thresholds/` 及本机同名备份目录。

2026-11-05 用户独立实例详情已实现，尚未部署：`/instances/:id` 提供概览、监控、GPU配置和任务；首页及初始化完成入口已切换，旧抽屉移除。GPU绑定信息由后端按实例冻结政策返回，前后端应一同发布，无数据库迁移。前端499条测试、类型检查与生产构建通过；本地真实Router和隔离MySQL浏览器验证首页进入、四张监控图、跨页签保留草稿、GPU策略PUT持久化、停止实例GPU卸载202及持久任务、375px卡片无横向溢出、404与管理员403。未运行原生GPU worker，不把受理称为物理卸载成功；隔离夹具的额度账户缺少完整政策，顶栏正确显示读取失败，未改动线上账户或实例。

上述成员详情已部署到 `.111` `/srv/101cloud/releases/20261105-member-instance-detail`，同时更新后端，无数据库迁移。旧程序、配置和原前端链接保留 `/var/backups/101cloud/20261105-member-instance-detail/`；旧前端为 `20261105-batch-delete-no-name`。发布HTML和后端SHA256与本地一致，HTTPS ready及服务active，Nginx配置未变。线上静态资源配合浏览器隔离只读响应验证四页签和概览显示，无页面横向溢出；未向生产提交实例或GPU操作。后端重启期间短暂502后恢复。

成员监控已移除曲线上方的查询范围、粒度/覆盖率/点数、可查范围与缺测说明整块文字；仅精简展示，采样、缺测断线及错误提示不变。类型检查、生产构建及浏览器隔离响应验收通过。已仅前端部署 `.111` `/srv/101cloud/releases/20261105-monitor-text-clean`；旧入口记录在 `/var/backups/101cloud/20261105-monitor-text-clean/frontend.link`。线上 HTML 校验和与本地一致、HTTPS ready 正常，后端二进制与 PID 未变，无服务重启或实例操作。

2026-11-05 系统类型图标卡片及版本下拉菜单已仅部署 `.111`，目录 `/srv/101cloud/releases/20261105-os-version-dropdown`；旧前端仍保留在 `/srv/101cloud/releases/20261104-password-or-public-key`，原入口与校验记录备份到 `/var/backups/101cloud/20261105-os-version-dropdown/`。线上 HTML 和选择器 JS 的 SHA256 与已验证构建一致，HTTPS ready 正常，后端程序校验和及 PID、Nginx 配置未变，无数据库迁移或服务重启。真实登录页可加载；浏览器使用线上静态资源及隔离目录/账户/报价响应验证系统卡片、版本下拉、具体镜像 ID 报价与 375px 无横向溢出，未向生产提交实例创建或其他业务写入。发布前已通过选择器 4 条回归、vue-tsc 及生产构建。

2026-11-04 已部署 `.111`（`/srv/101cloud/releases/20261104-stopped-resources-free`）：管理端磁盘计费仅可选“仅运行时计费”或“磁盘免费”。表单说明确认停机后CPU、内存、磁盘、GPU全部免费，GPU可保持绑定且继续占用容量、不自动释放，再次确认运行后恢复应计费资源。停止失败/状态未知不能提前免计费；节点离线并非已停止，离线政策不覆盖确认停机免费规则。历史停机前费用不追溯退款，未交付创建失败退款规则保持。健康接口及真实umi停机计费稳定性验证通过，已发布前端配合隔离管理响应验证策略选项和说明，未提交配置修改。

管理员操作不填写、提交或新增记录操作理由；基础审计仍保留操作者、动作、时间，历史记录不删除。确认弹窗使用无参回调，保留危险操作提示、删除名称确认及强制操作风险确认；CSRF、幂等键和 If-Match 不变。控制台选择适用类型后连接，无理由输入。自定义实例需求在平台外沟通，平台仅保留管理员在实例页直接下发；不提供成员申请、管理员审批、申请说明或审批意见界面，旧申请地址不保留兼容跳转。

管理员实例列表支持当前页复选框多选、全选、清空，以及批量启动、正常停止、删除。翻页或修改筛选清空选择；轮询保留仍在当前页的选择。确认窗口冻结实例、名称与 revision，按能力和状态逐项标明可提交/跳过；操作中、未就绪、状态过期或不支持动作的实例不下发。异常实例的强制清理仍在详情页单独确认。

批量删除无需逐项输入实例名，仅需勾选不可恢复风险并确认删除；单实例详情页名称确认不变。运行中实例沿用强制关闭及数据丢失确认，停止实例不额外强停。每项独立幂等键、If-Match 和现有 CSRF 请求路径，不新增后端批量接口。逐项提交，某项失败不阻断其余项；区分已受理、提交失败、受理结果待核对、跳过。已受理任务交任务中心追踪并刷新列表，不把受理当作执行成功；不确定结果不自动重发。

2026-11-05 管理员实例批量操作已仅前端发布 `.111`，目录 `/srv/101cloud/releases/20261105-admin-instance-batch`，原入口及配置/服务基线、构建归档保存在 `/var/backups/101cloud/20261105-admin-instance-batch/`。类型检查、变更文件 oxlint、生产构建通过；线上静态产物配合隔离 API 响应验证上述操作、375px 列表/删除弹窗及暗色弹窗。HTML SHA256 与本地构建一致，HTTPS ready 正常，后端与 Agent 二进制、PID、Nginx 配置未变；无数据库迁移、服务重启或真实实例操作。

2026-11-05 批量删除已移除逐项名称输入，仅保留一次风险勾选及删除确认；发布 `.111` `/srv/101cloud/releases/20261105-batch-delete-no-name`，旧入口记录在 `/var/backups/101cloud/20261105-batch-delete-no-name/`。类型检查、变更文件 oxlint、生产构建通过；线上静态资源配合隔离 API 验证无名称输入、确认后两项分别受理，未删除真实实例。后端/Agent 二进制及 PID、Nginx 配置未变，HTTPS ready 正常。

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

- 首页点击实例名称进入独立详情，移除卡片底部“进入实例”和“查看初始化”按钮；初始化页面保留在创建流程。每个内网 IPv4 后提供复制按钮和成功反馈。已仅前端部署 `.111` `/srv/101cloud/releases/20261105-member-home-name-copy`，旧入口记录于 `/var/backups/101cloud/20261105-member-home-name-copy/frontend.link`。保留此前 GPU 空闲配置精简；线上静态资源配合浏览器隔离只读响应验证名称导航、复制成功及旧按钮移除，无运行错误或业务写入。HTML校验和一致、健康接口正常，后端程序/PID及Nginx配置未变，无服务重启。
- 首页卡片轻度紧凑版已仅前端发布 `.111` `/srv/101cloud/releases/20261105-compact-member-cards`：内边距16px、区块间距12px、系统图标32px，保留信息和名称字号。类型检查/构建通过；线上静态资源配合隔离响应实测padding16px、gap12px。健康正常、后端PID未变；旧入口保存在 `/var/backups/101cloud/20261105-compact-member-cards/frontend.link`。
- 卡片横向收窄版已发布 `.111` `/srv/101cloud/releases/20261105-narrow-member-cards`：1200px起三列，间距20px；平板两列、手机单列，内部16px留白不变。类型检查及构建通过，线上静态资源配合隔离数据在1440px实测三列约381px、gap20px、无横向溢出，健康接口正常。旧入口位于 `/var/backups/101cloud/20261105-narrow-member-cards/frontend.link`，仅更新前端。
- GPU监控修复尚未部署：按各字段独立显示SM/归因显存/进程/引用数，不因可选SM或引用扫描不完整隐藏服务端已返回的有效占用；停止实例说明无运行态采样，历史保留、未知不补0，原因码有中文解释。显示与安全卸载门禁分离，过期、不完整归因或引用扫描不能授权在线卸载。新增29条行为回归及全量533条测试、类型检查和构建通过；浏览器验证172MiB/1进程可见且卸载禁用。节点独立临时Agent的真实CUDA验收见host-agent README，未部署或改动业务实例。
- 上述修复已部署 `.111` `/srv/101cloud/releases/20261105-gpu-monitor-repair`，同步更新 Host Agent；未改后端和数据库。Agent/backend active，ready正常；线上Agent全局快照200、collection_complete=true、采样序号递增。线上静态资源配合隔离只读响应验证172MiB/1进程显示和SM未知，无运行错误或业务写入。旧前端、Agent程序/配置及发布脚本保存在 `/var/backups/101cloud/20261105-gpu-monitor-repair/`。采集仍如实保留进程扫描不完整、缺样本、停止实例归因未知等原因，不宣称所有质量标记已消失。
- GPU空采样后续修复未部署：正常无context且NVML无增量SM样本显示“暂无活动采样”，明确0进程/0引用/0显存且已配置的Running样本说明“GPU已挂载，尚无GPU程序活动”。Stopped缺运行态进程用中性说明；真实unsupported、未知归因及驱动错误继续警告，历史未知不改写，卸载门禁不放宽。567条前端测试、类型检查和构建通过；真实运行挂卡实例在临时Agent连续5点验证正常空状态，采样原值回放浏览器确认没有错误级警告。需要同步发布Agent、后端原因白名单及前端，无数据库迁移。
- 上述空采样分类已同步发布Agent/后端/前端至 `.111` `/srv/101cloud/releases/20261105-gpu-empty-sampling`，无迁移。真实运行实例42的最新GPU明细已入库：process_count/reference_count/memory=0、SM=NULL、原因仅 `gpu_utilization_no_samples`；线上静态资源搭配只读隔离响应验证中性空采样说明、无错误警告及无业务写入。两个服务active、ready正常，HTML/程序SHA匹配；后端重启期间短暂502后恢复。旧程序、配置、前端入口、数据库一致性备份及发布脚本位于 `/var/backups/101cloud/20261105-gpu-empty-sampling/`。

- 登录及根地址按 `/me.user.role` 分流；登录回跳只允许当前角色已开放的站内页面。退出或重新登录切换账号/角色会清理前一账号的状态。任务中心按角色分别查询本人 `/tasks/{id}` 或管理员 `/admin/tasks/{id}`，异步旧响应不会污染新账号。
- `/instances` 使用本人接口，每页 20 条，支持运行状态、产品类型筛选、刷新和游标上一页/下一页。卡片展示实例名、ID、类型、组合状态、分配 CPU/内存/磁盘、状态过期及额度停机提示。GPU 分配通过 `/instances/{id}/gpu-allocations` 独立加载，按型号及分配状态汇总数量，不把历史释放/取消记录算作当前分配。
- 卡片复用 `OsIcon` 及已有本地系统 SVG，依据实例接口 `image_os_name/image_os_release` 选择真实来源镜像图标，停用/退役镜像不影响已有实例展示。GPU 的 `configured` 表示挂载配置，`observed` 表示进程上下文/设备引用；已配置但空闲时仍显示“已挂载”，不因 `observed=false` 误报未挂载。
- 规格下方显示“内网 IPv4”，读取本人 `/instances/{id}/network` 并仅展示已绑定的 RFC1918 私网地址。网络异步加载，失败保留旧值并提示，过期状态有标记；未绑定及仅预留地址不冒充可用地址，无地址显示“未分配内网 IPv4”。
- 顶栏通过 `/credits/account` 显示 `available_pt`（Pt），浮层展示 `remaining_pt`、`reserved_pt`、`outstanding_debt_pt`、服务端停机提示及手动刷新；页面可见时每 30 秒更新。剩余为内部余额扣除未偿欠款，可用再扣除预留；前端原样显示带符号的精确十进制字符串，不钳制负数，不重复扣除欠款或预留。额度失败不显示为 0，已有值保留并明确标记更新失败；实例列表及 GPU 刷新失败同样保留上次结果并提示。
- 首页“创建实例”进入弹性容器配置页；配置页按钮只跳转 `/instances/create/confirm` 核对订单，最终确认页按钮才提交创建。配置支持精确资源滑块/输入、兼容镜像、GPU及网络，服务端实时报价与前端BigInt估时。点击实例卡片进入 `/instances/:id` 独立详情页，提供概览、监控、GPU配置和任务四个选项卡；不再使用首页详情抽屉。账单、通知及个人设置仍未开放。
- GPU 空闲回收固定要求完整、新鲜的归属证据，以及实例 GPU 进程、设备引用、显存占用全部为零；不提供可配置的利用率或显存百分比门槛。每个实例独立累计连续空闲时间并使用自身空闲时长，政策仍限制可选时长和动作。管理端政策表单、管理端及成员 GPU 面板与后端同步移除这两项配置；严格安全规则、实际 GPU 指标显示及 GPU 调度高低负载阈值保留。
- 创建页按系统类型展示图标卡片，版本通过下拉菜单选择；选项区分版本、架构及镜像名称，GPU 不兼容版本不可选。报价和创建仍使用具体镜像 ID；刷新目录时尽量恢复原选择。
- 2026-11-03 初始化窗口已发布到 `192.168.0.111`，当前前端目录 `/srv/101cloud/releases/20261103-initialization-window`。前后端、配置及数据库备份在 `/var/backups/101cloud/20261103-initialization-window/`，`frontend.link` 记录原发布目录。本次无需新增迁移；HTTPS 健康接口及真实成员任务 97 的阶段接口/窗口只读验证通过，临时验收会话已撤销，未创建或重跑实例。历史任务缺少最终核验标记会保守显示初始化尚未完成，不代表本次实际重新执行或初始化失败。
- 2026-11-04 初始化错误响应已发布到 `192.168.0.111`，当前目录 `/srv/101cloud/releases/20261104-initialization-errors`；前后端、配置、数据库备份在 `/var/backups/101cloud/20261104-initialization-errors/`。无新增迁移，后端构建校验和一致，HTTPS ready 正常；真实发布资源搭配浏览器隔离诊断响应验证了具体 ssh-keygen 原因、退出码、整体待核对与重复错误去重。未重跑初始化或修改历史任务；旧任务不会自动补写诊断。
- 2026-11-04 精简初始化及失败弹窗已仅前端部署 `.111`，当前目录 `/srv/101cloud/releases/20261104-initialization-failure-modal`；原入口记录在 `/var/backups/101cloud/20261104-initialization-failure-modal/frontend.link`。后端校验和未变、健康接口正常；发布资源配合隔离诊断响应验证主页面无摘要和原因、unknown 显示失败、点击失败按钮可查看原因和原退出码。未改动实例或任务。
- 2026-11-04 失败创建自动回收及财务冲正已部署 `.111`，发布目录 `/srv/101cloud/releases/20261104-failed-creation-cleanup`，备份 `/var/backups/101cloud/20261104-failed-creation-cleanup/`。执行内嵌迁移 `20261104_00_failed_creation_cleanup.sql`；历史 Arch 实例29已由任务108完成清理，GPU确认释放、IP解除、计费会话全部结束。已按原流水冲正81.381546 Pt（14304笔），相关账单取消及预留释放完成；成员详情404、列表无该实例，原任务102错误页真实浏览器验证可打开。上线核验修复旧GPU步骤仅存operation UUID的兼容，未重放原初始化。仅结果不明的其他历史任务未擅自删除。

确认页只读展示配置、费用明细、额度、首窗口预留和报价倒计时，最终提交前复核报价/账户；条款变化只更新展示，必须再次确认。返回修改保留选择。冻结快照仅当前会话持有，无快照或刷新后提示重新选配，不生成虚构订单号/支付记录。不确定结果锁定原payload/key重试；受理后使用任务 ID 跳转 `/instances/initialization/:taskId`，换号不会泄露前一账号配置。

报价和预计时长只依赖资源等配置，不要求也不发送名称或 `login`；输入、清空名称或修改、清空 root 密码不重新报价、不清空时长。前往确认和真正创建要求有效名称及 root 密码。可用余额不重复扣预留，估时不包含其他实例消耗及未来额度变化，也不承诺实际到期时间。

初始化窗口：居中最大 720px，三点仅为标题栏装饰；显示实例名称、任务 ID 和真实时间戳耗时，不再显示系统/资源/安全登录摘要。通过本人 `GET /tasks/{id}` 的 `creation_progress.stages` 展示真实阶段；GPU 不适用时省略，脚本不适用明确跳过。底部细线只表示活动，不表示百分比；不展示日志、脚本输出或模拟命令。精简及失败弹窗改动已部署 `.111`。

- 初始化成功后的“进入实例”进入 `/instances/:id` 独立详情页；页面核验当前账号和实例归属，不自动跳走。详情页异步请求均按账号、实例和请求代次隔离，实例不存在或无权访问时清空旧内容。

### 创建时的 root 登录凭据

- 两步创建沿用现有布局；登录方式二选一：默认“root 密码”或“SSH 公钥”，仅显示所选方式的输入框。密码方式要求两次输入一致，8–128 个 Unicode 字符，保留首尾空格并禁止控制字符；公钥方式不要求密码。面板仅保留简短限制、错误提示及“SSH 服务需自行安装。”，确认页仅展示所选方式的密码掩码或公钥数量。
- 公钥方式每行一条，忽略空行；1–10 条（去重前），每条最多 8192 UTF-8 字节（含注释）。支持标准 Ed25519、RSA（至少 2048 位）及 ECDSA nistp256/384/521，拒绝私钥、authorized_keys 选项、证书及无效内容。前端检查语法、wire 结构与长度，移除注释并按规范化公钥去重；服务端负责最终密码学公钥校验。
- 最终 `POST /instances` 只发送所选方式：`login: {root_password}` **或** `login: {ssh_public_keys: string[]}`，绝不同时发送；未选方式即使留有无效草稿也不参与校验或提交。所有 `POST /quotes` 显式排除 `login`，登录方式及凭据不影响资源报价。确认页不展示密码、公钥、注释或指纹原文。
- 私有草稿记录登录方式、密码及公钥文本，确认时仅冻结所选登录配置；公开状态仅有所选方式的密码是否设置或公钥数量，以及 `hasPendingIntent`。不写 local/session storage、URL、日志或 Pinia action 参数，普通 `form/order/$state` 不保存凭据。返回修改恢复模式及敏感草稿；修改草稿或模式后前进到旧确认页不会替换已冻结凭据。结果不确定时使用同一原始 body/key；受理、退出、换账号、角色切换及显式重置清空敏感引用。
- 凭据配置依赖所选节点/项目就绪镜像的 cloud-init 能力。密码方式设置 root 密码；公钥方式写入 root 的 `authorized_keys` 并禁用密码认证凭据。用户公钥不同于 SSH 主机密钥；此流程不生成用户密钥或主机密钥，不安装、配置或重启 SSH 服务。管理员初始化脚本保持独立，管理员自定义创建继续不传 `login`，导入不接收登录配置。最终创建错误不回显服务端输入内容。
- `instance-login.test.ts` 覆盖两种方式、忽略非所选输入、密码校验、公钥规范化/去重/数量/字节限制及节点镜像能力；`instance-order.test.ts` 覆盖两种互斥 payload、确认模式冻结、公开状态/报价隔离、两种方式原体原键重试、账号清理与旧响应隔离。本次未执行检查；统一验收 login/order/locales/initialization 测试、前端 typecheck/build 及浏览器两步流程。选择器：`#creation-login-mode`、`#creation-root-password`、`#creation-root-password-confirmation`、`#creation-ssh-public-keys`；确认摘要：`[data-testid="root-password-summary"]` 或 `[data-testid="ssh-public-key-summary"]`。
- root 密码或请求的公钥实际核验失败沿用 `initialization_password_not_ready` / `root_password_not_ready`，文案明确可能是密码未正确设置（包括首次登录强制改密）或所提供公钥未正确写入；不把 cloud-init done 等同于凭据就绪，也不把凭据就绪等同于 SSH 可登录。仍可能出现的 `initialization_marker_missing` / `completion_marker_missing` 只指 cloud-init 启动完成标记，不是旧 SSH 登录模式标记；历史错误保持可读，不触发重放。


## 可读错误与任务失败

- 同步 API 和异步任务/步骤共用 `locales/errors.ts` + `ErrorFeedback`：优先显示后端安全、具体的中文原因，历史英文 message 用中文错误码表兜底，未类型化异常不回显原文。主标题短，资源数字、字段校验与中文处理建议分开展示；已写进原文的同一数量不会重复堆叠。
- `details` 显示配额/节点/常驻/存储池/IP/GPU 范围和资源维度，以及已用、本次申请、上限/物理总量、可用量。字节走 BigInt 的 `formatBytes`，计数保留十进制字符串，pt 只去尾零；错误码/阶段在技术详情，请求 ID 独立显示、可复制。
- 任务 API 通过 `TaskSchema` / `TaskStepSchema` 校验可选可空 `error: {code,message,details}`，兼容保留 `error_code` 的历史记录。任务中心通知、任务列表/详情、实例任务标签页与创建失败横幅展示具体原因及建议，而非只显示 `unknown` 或错误码。
- `unknown` 仍为结果待核对，绝不自动重放；`failed` 无盲目重试按钮。只有 `resource_reservations_released=true` 的实例显示已确认创建前失败及 IP/GPU/计费预留释放，并优先显示创建失败；false/缺失明确提示预留未确认释放。已就绪实例仍以真实 `state_stale` 显示状态未知，不伪造运行状态。
- 登录表单、控制台准备和指标查询保留完整错误说明与请求 ID；CSRF 原键重试、If-Match、幂等键和危险操作确认规则保持不变。历史任务与真实实例不会因前端展示更新而删除、重置或重试。
- 初始化诊断使用独立白名单：`initialization_cloud_init_degraded/cloud_init_failed/tool_missing/marker_missing/script_failed/timeout/result_unknown/diagnostic_unavailable`（每项均带 `initialization_` 前缀）。固定中文标题不回显上游 message；只展示整数 `exit_code`、固定分类 `reason`、固定 `cloud_init_status` 的中文映射及 `login.cloud-ready/init.cloud-ready/init.template` 中文阶段。未知值与任意 action、日志、密码、哈希、user-data、环境等 details 字段均不展示；`done` 不代表所有初始化步骤成功，不改变放行策略。
- 初始化窗口只在 failed/unknown 时提供“查看失败信息”按钮；点击弹窗查看去重后的任务/步骤安全错误及必要的核对说明，主页面不展开失败原因或技术详情。成功、执行中、排队或仅网络查询异常不显示此按钮。账号/任务切换或离开失败状态会关闭弹窗，不改变后端状态或重试规则。
- 行为边界测试：`apps/web-antd/src/locales/errors.test.ts` 覆盖八类固定码、降级但 done、整数退出码、未知/原型键过滤、敏感文本不泄露与 unknown 文案；`views/member/instances/initialization.test.ts` 覆盖 failed/unknown 步骤原因与禁止进入。统一验收建议：`pnpm exec vitest run apps/web-antd/src/locales/errors.test.ts apps/web-antd/src/views/member/instances/initialization.test.ts`，以及应用 typecheck/lint/build。
- `command_terminated` 仅表示命令被终止：退出码 137 不直接判定超时，提示管理员核对超时/资源限制等原因；`template_timeout_invalid` 表示模板超时配置无效，两者仍归初始化脚本失败。对应边界测试保留原整数退出码，不混同已确认 timeout。
- degraded 明确显示“cloud-init 已带警告完成”，与 cloud-init 错误失败区分。历史 `ssh_host_key_generation_missing_tool` 仍说明缺少 ssh-keygen，但提示检查镜像 cloud-init SSH 模块配置，不再将安装 OpenSSH 作为网络/root 密码配置前提；普通缺工具仍使用 `required_tool_missing`。该特定原因有独立白名单/敏感文本过滤测试。
- 后端任务 unknown 仍保留待核对及禁止自动重放语义，但初始化页面标题与对应阶段统一显示“初始化失败”/“失败”；核对说明仅在失败弹窗中显示。错误按安全格式化后的 code、中文 stage、标题及说明去重，不合并不同阶段的诊断。
- 未交付创建失败清理契约（已部署 `.111`）：任务可包含 `creation_cleanup: {status: pending|cleaning|unknown|completed, billing_status: pending|refunded, instance_name: string}`。schema 校验固定状态和安全名称，保留原 `instance_id`、`creation_progress`、任务/步骤错误及原 unknown 结果；前端不把待核对改成已知失败。
- 存在清理标记时，初始化页只从任务读取安全 `instance_name`，清空旧实例摘要并停止请求实例详情；实例 API 隐藏后的 404 是预期，不再显示“实例摘要读取失败”。未标记任务遇到非预期摘要查询错误仍正常报错。清理状态与退款状态、未交付及等待清理期间不收费说明仅在“初始化失败信息”弹窗呈现，不堆放在主页面；原阶段和原错误继续可读。
- 清理未完成或退款尚待处理时只轮询原任务；不重试创建、不显示“进入实例”，也不提示重放原任务。任务 `next_action=wait_cleanup/none/inspect_failure` 不等于可重新提交原创建。实例列表完全依赖后端过滤失败记录，不按前端 provisioning 状态另行隐藏；隐藏实例的直接详情链接遇到 404 只显示不可访问错误，不回退到卡片或旧配置。
- 新增/扩展行为测试覆盖清理 schema、禁止进入、安全任务摘要、跳过实例 404、失败弹窗状态、退款轮询、直接详情 404 和任务入口保留。前端全量 475 项测试、类型检查及生产构建通过；使用真实本地 API/worker 清理响应在浏览器验证错误页仍可恢复，清理/退款信息仅在失败弹窗展示，且不再读取隐藏实例详情。


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
corepack pnpm exec vue-tsc --noEmit --skipLibCheck   # 类型检查
cd ../.. && corepack pnpm exec oxlint apps/web-antd/src
```

生产构建统一从完整仓库的 `backend/build.sh` 进入（先安装上述前端依赖；脚本不安装或下载依赖）：

```bash
cd ../backend
./build.sh
# Linux amd64；ARM64 将 GOARCH 改为 arm64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ./build.sh
```

脚本从任意工作目录调用均可，执行 `corepack pnpm --dir apps/web-antd exec vite build --mode production`，通过 CLI 输出目录在临时目录生成全新 bundle，校验真实 `index.html` 与 `_app.config.js` 后替换 `backend/internal/webui/dist/` 并编入 Go 二进制。生成资源不提交版本库，无 placeholder 或磁盘静态资源回退；Vite 本地开发配置不变。默认输出 `backend/bin/101cloud-backend`，额外 Go 构建参数可传给脚本，例如 `./build.sh -o /tmp/101cloud-backend`。

首次检出后需先 `./build.sh --frontend-only` 准备嵌入资源，才能在 backend 运行 `go test ./...` 或直接 `go build`。详见 [后端部署说明](../backend/README.md)。

内嵌方式本地验收：统一原生/Linux amd64构建成功，仓库外只带二进制/配置/密钥启动真实后端，浏览器登录页可用且深链接正确回到鉴权流程；366个真实前端资源HTTP内容一致。SPA、静态资源404、API JSON404/405、HEAD和缓存回归及全量真实MySQL后端race/vet通过。线上仍使用原独立静态资源入口，尚未切换。

## 生产部署

构建产物只请求同源相对路径 `/api/v1`，不含任何后端地址。前端与 API 由同一个 Go 二进制提供；发布/更新只替换该二进制，不再上传独立静态目录。仍需数据库、运行配置及原 master key，Host Agent 独立部署/更新；不自动执行迁移，数据库升级须显式执行 `migrate`。当前线上尚未切换到内嵌方式，以下是新的部署示例，不是已部署记录；历史记录不作改写。

Nginx 在同一 HTTPS 域名下反代整个后端（移除旧静态 `root`/`try_files`）：

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    server_name cloud.example.com;           # 必须与后端 CLOUD_ORIGIN 完全一致
    # ssl_certificate / ssl_certificate_key ...

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;     # 终端 / VNC WebSocket
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        proxy_buffering off;
        proxy_cache off;
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

2026-11-04 管理实例详情增加卡住实例“强制清理删除”：操作中、状态过期及未就绪时仍可打开，精确名称和风险确认后仅管理端发送 `cleanup_stuck=true`、`mode=force`、`confirm_data_loss_risk=true`、`delete_data=true`。弹窗说明取消已有操作、磁盘不可恢复、权威确认原生不存在则跳过原生操作、离线不算不存在，并保留历史计费/审计。普通就绪删除保持原请求行为；其他生命周期操作不因此放开。轮询期间表单可升级为清理确认，要求重新输入名称和勾选；202 只提示提交，任务完成刷新、详情确认删除后返回列表。新增 `views/admin/instances/detail.test.ts` 行为回归；本条不代表已执行检查或已部署。

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

## 普通用户弹性容器创建

2026-11-02 已仅发布 `.111` 前端 `/srv/101cloud/releases/20261102-member-create`。首页“创建实例”进入 `/instances/create`，侧栏仍仅有“实例管理”。套餐、精确资源滑块/输入、节点/项目兼容镜像、GPU及网络配置接通真实目录与报价；前端用可用Pt除以当前每小时报价估算时长，不新增后端时长接口，不作为创建准入依据。创建受理后返回首页，成员任务通过 `/tasks/{id}` 跟踪，结果及额度自动刷新。

验证：类型检查、72条用例、oxlint和生产构建通过。隔离MySQL、真实后端Router/worker与有状态TLS Agent协议夹具中，浏览器以4 vCPU / 8 GiB / 40 GiB实际提交创建，任务经过preflight/create/start后成功，数据库确认ready/running及精确字节、plan_revision和policy_versions。模拟服务端已受理但响应丢失后，同键同payload重试仅新增一个实例；报价过期只重新报价，套餐revision变化返回409后重新确认，不自动创建；慢报价不能覆盖新资源价格；非法资源输入立即作废报价/时长。桌面明暗布局及390px窄屏单一底部提交栏已检查，无页面横向溢出。TLS夹具不创建真实硬件或Incus容器。

线上以零配额临时普通用户 `deploy-create-ui-20261102`（ID7）验收真实入口、套餐/镜像/资源选项及14 Pt/小时报价，配额不足时提交正确禁用，未请求 `POST /instances`；该账号已停用，成员与管理员验收会话均退出。未在测试服务器创建或操作实例，未修改既有套餐/政策/配额。仅更新静态前端，后端程序、数据库结构及Nginx配置未改，无服务重启。旧版本 `/srv/101cloud/releases/20261102-editable-plans` 保留，回滚记录 `/var/backups/101cloud/frontend-before-member-create-20261102.link`。


2026-11-02 修复“未输入实例名称无法估算时长”，仅部署 `.111` 前后端。报价移除重复的创建名称校验，创建准入仍独立校验名称；前端将配置报价与创建元数据分开，不使用临时或默认名称代替用户输入。前端发布目录 `/srv/101cloud/releases/20261102-quote-name-independent`，旧链接 `/var/backups/101cloud/frontend-before-quote-name-20261102.link`，旧后端 `/var/backups/101cloud/backend-before-quote-name-20261102`；无数据库结构变更，后端更新并重启，Nginx及Host Agent未改。

后端全量race/真实MySQL回归及vet通过：无名称、空白和超长名称仍可配置报价，但对应创建均422且不预留资源或排队任务，填写有效名称后创建保留该名称。前端类型检查、72条用例、oxlint及构建通过。真实普通用户页面空名称时以100 Pt、14 Pt/小时显示约7小时8分；输入/清空名称保留原报价和时长，创建按钮仅在名称有效时可用；新报价请求未包含name。验收只授予临时账号 `deploy-unnamed-quote-20261102`（ID8）最小测试配额，未请求线上创建；结束后配额归零、账号停用、验收会话退出，未修改既有用户配额或实例。


## 创建前确认订单

2026-11-03 已仅发布 `.111` 前端 `/srv/101cloud/releases/20261103-order-confirmation-r2`。配置页“创建实例”只进入 `/instances/create/confirm`，不请求创建；确认页核对系统/套餐/名称/资源/GPU/网络、小时费用、额度/预留、估时及报价倒计时，最终按钮才创建。返回修改保留全部选择。确认信息只在当前账号会话内保存，刷新或直接访问无快照时提示重新选配；无后台订单、支付或包月模型。

最终确认前复核报价和账户，经济条款变化只更新展示，要求再次确认；过期只更新报价。套餐变化返回配置且保留原选择供用户校正。不确定请求冻结原payload/key，后退也不能覆写；重试鉴权失败不丢弃原结果未知的意图，换号/退出清理旧状态，旧响应不污染新账号。前端类型检查、89条用例、oxlint及生产构建通过。

隔离MySQL配合现有测试身份中间件和只读TLS存储夹具，浏览器确认配置页/过期/改价/套餐变更均不创建，返回保留名称及3 vCPU/4 GiB/10 GiB配置；最终4 vCPU/4 GiB/10 GiB请求受理为202。模拟已受理响应丢失后同键同体重试，数据库仅1条实例和1条queued任务，成员任务查询200。未运行worker或硬件容器；不将该验收称为原生创建成功。390px布局无页面横向溢出，移动端只有一个最终操作栏；未填名称的估时行为保持正常。

线上临时普通用户 `deploy-order-confirm-20261103`（ID9）验证真实确认页、14 Pt/小时及约7小时8分估时、单一菜单和返回后1 vCPU/1 GiB/5 GiB选择保留，未发送线上创建请求；临时配额已归零、账号停用、验收登录退出。只更新静态前端，不改后端、数据库结构、Nginx或Host Agent，无服务重启，不处理登录/磁盘故障。前一版本与旧链接保留：`/var/backups/101cloud/frontend-before-order-confirmation-20261103.link`（功能发布前）及 `frontend-before-order-confirmation-r2-20261103.link`（r1）。


2026-11-03 管理员额度调整结果说明及卡顿修复仅发布 `.111`：发放确认前说明历史欠款优先抵扣，成功后持续展示发放/扣减金额、本次偿债、提交后可用与未偿欠款，不再只提示“已完成”。使用后端提交后的新余额和revision，不将旧额度桶快照作为余额；入账成功但详情刷新失败时保留结果并明确提示勿重复提交。关闭重开抽屉保留同一页面内各账户的最近结果，刷新整个页面不作本地持久化。抽屉宽度随窄屏收缩，390px无内容截断。

类型检查、oxlint、生产构建通过；真实后端与隔离MySQL浏览器验证6001笔欠费下有/无过期各发放22pt约1秒，余额为0时明确显示已偿债22pt；无欠费账户可用实际增加22pt；模拟详情刷新503保留提交结果，未重复发放。明暗主题和390px布局验证通过。线上仅打开真实账户确认提示并取消，无测试发放/扣减、免债或实例操作，页面无运行时错误；前后端服务及财务一致性见 `../backend/README.md`。前端目录 `/srv/101cloud/releases/20261103-credit-adjustment`，旧链接备份 `/var/backups/101cloud/frontend-before-credit-adjustment-20261103.link`。

2026-11-03 负余额展示源码更新（未在此步骤构建、测试或发布）：成员及管理员账户详情增加未偿欠款，额度调整响应同时更新缓存中的欠款；发放提示说明欠款未还清时余额可能仍为负数。账户与调整响应的剩余/可用均为扣除欠款后的净额，预留仅扣除一次；现有创建确认及提交门禁按精确 BigInt 比较，负可用额度不能支付首窗口，包括零价窗口。内部额度桶、偿债、计费及停机逻辑不由前端改写。

2026-11-03 后台分批偿债前端契约源码更新（本步骤未运行检查、构建或部署）：`CreditAccount` 与 `CreditAdjustmentResult` 增加必填 `settlement_pending`。管理员确认说明“提交即入账、历史欠费后台抵扣”；结果在 pending 时显示“已入账，历史欠费正在后台分批抵扣”，不把发放成功说成偿债完成。`settled_debt_pt` 仅展示入账事务内真实偿还额，无同步偿债即为 0。提交时的欠款总额可能暂未下降，净余额已反映新发放并保留负值，不能再次扣减总欠款。账户列表、账户抽屉、用户详情额度标签和成员额度浮层提示当前后台处理状态；结果快照与当前账户分别展示。沿用成员现有 30 秒刷新，账户抽屉提供手动刷新，不增加无限轮询；详情刷新失败保留真实入账快照与新余额，提示勿重复提交，刷新成功清除该失败提示。沿用原页面、幂等重试及取消逻辑，无新页面或自动重复发放。

2026-11-03 抵扣状态提示修复已仅发布 `.111`，目录 `/srv/101cloud/releases/20261103-settlement-status-live`，旧链接保留在 `/var/backups/101cloud/frontend-before-20261103-settlement-status-live.link`。历史发放结果仅显示“已入账”和“提交时已安排（非当前状态）”，不再用固定快照声称后台一直运行。账户抽屉仅在打开、页面可见、当前账户确有pending且无确认表单/已有刷新时每2秒读取详情；完成后停止请求并刷新列表，关闭/切换账户忽略迟到轮询，沿用统一usePolling卸载/隐藏生命周期。失败时保留已入账结果、提示当前进度未知并自动重试，不将缓存状态冒充实时进度。

vue-tsc、变更文件oxlint及生产构建通过。线上只读核查真实账户欠款0且pending=false；浏览器拦截响应模拟pending→详情503→完成，确认自动恢复、完成后超过两次刷新间隔不再请求、历史结果不声称正在处理且未重复POST。模拟POST仅在浏览器返回、不送达后端，前后管理员实际发放笔数和总额一致。此验证针对状态展示，不冒充真实资金发放。仅更换静态前端，无后端重启或财务数据修改。

2026-11-03 套餐持有显式政策绑定：政策页面移除平台默认标记、切换按钮及绑定 API；节点 GPU 仅配置硬件状态/名额，价格由套餐选择，同型号可选不同价格。套餐、自定义下发和纳管逐项选择计算定价、实例额度规则、监控及适用的 GPU 政策；实例绑定不会随套餐编辑而改写，历史分配保留费率快照。节点新建/概览编辑选择宿主监控政策。创建用户显式选择独立的钱包发放政策；缺少钱包时进入“用户 → 用户详情 → 额度”选择政策创建，不自动赠送余额。共享钱包的多个政策停机事件分别显示。上述为实现说明，本次未执行检查、浏览器验收或部署；历史发布记录中的默认绑定行为已由本次切换取代。

历史自定义实例首次增加 GPU：进入“实例详情 → GPU”，缺少 GPU 定价时先显式绑定配置档，再执行挂载。仅没有任何历史分配记录的实例允许补设；使用实例 ETag 调用 `PUT /admin/instances/:id/gpu-pricing-policy`。GPU 候选展示使用实例所绑定的价格，不使用任意同型号配置档的首个价格。

2026-11-03 上述套餐持有策略切换已仅部署 `.111`，目录 `/srv/101cloud/releases/20261103-plan-owned-policies`。前端全量Vitest、vue-tsc及生产构建通过；独立Chrome实际确认政策页无平台默认入口、节点GPU编辑无价格字段，用户切换套餐显示对应4/7pt价格，桌面和390px布局已检查。真实后端在隔离库完成两次报价/202准入，同一GPU持久化4/7pt费率；原生交付worker未运行，不冒充硬件交付。线上登录确认旧默认接口404及新政策页面，报价不创建、不预留资金；无真实用户测试发放、免债或实例创建。旧前端链接保存在 `/var/backups/101cloud/frontend-before-20261103-plan-owned-policies.link`。

2026-11-03 列表表头与筛选栏重叠修复已仅发布 `.111` 静态前端 `/srv/101cloud/releases/20261103-list-toolbar-alignment-r2`。线上实测Ant Space wrap产生-8px底部margin，覆盖外部mb-3并使表格与筛选控件重叠；实例、目录（镜像/套餐/模板）、财务（账户/流水/兑换码/额度事件）、任务、审计的列表工具栏改为正常Flex换行布局与12px表格间距。筛选控件显式固定宽度以避免组件width:100%使每项占满一行；缺少滚动配置的列表增加表格内部横向滚动，日期范围限制容器宽度，不靠负偏移或全局覆盖Ant组件。

vue-tsc、十个变更文件oxlint及生产构建通过。真实浏览器以1440px/390px遍历上述十个列表：间距12px，有数据的表头/首行单元格横坐标及宽度差0px、无页面横向溢出；空列表不虚报单元格测量。审计筛选无结果→清空恢复、窄屏表格滚动160px后表头与内容仍对齐，无运行时错误。仅GET列表及登录/退出操作，没有业务数据变更，不重启后端。原套餐配置版本及r1链接分别保存在 `/var/backups/101cloud/frontend-before-20261103-list-toolbar-alignment.link` 与 `frontend-before-20261103-list-toolbar-alignment-r2.link`。

2026-11-04 管理员卡住实例清理入口已仅发布 `.111`，目录 `/srv/101cloud/releases/20261104-admin-stuck-cleanup`。实例详情在操作中、状态过期或未ready时也可打开“强制清理删除”，须精确名称及风险勾选，发送cleanup_stuck=true并强制删除确认；普通ready删除保持原行为。仅受理202不宣称完成，任务确认已删除/详情404后返回列表；普通离线错误不当成已删除。真实隔离后端浏览器走通unknown任务接管至缺席清理成功，线上仅打开实例15确认后取消，没有真实删除。新增8条行为回归及完整49文件483条测试、vue-tsc、变更文件oxlint和生产构建通过；历史账单/审计保留。后端竞态及备份证据见backend README。

2026-11-04 密码唯一创建入口已仅发布 `.111`，目录 `/srv/101cloud/releases/20261104-password-network-cloudinit`。移除SSH公钥模式/解析/请求字段；新建仅输入root密码与确认，确认页固定掩码，密码仍只存账号会话内存并保留未知请求的原体/幂等键。文案明确root账号密码可用不等于SSH服务就绪，管理员脚本单独执行；旧实例登录模式仅只读展示，不重置密码。50文件483条测试、vue-tsc、生产构建通过；真实浏览器验证无公钥输入、密码确认掩码、报价无凭据及390px布局，未提交创建。后端使用没有ssh-keygen/sshd的真实Arch容器验证网络与密码成功，证据及备份见backend README。模板和既有实例未修改。

2026-11-04 登录方式最终改为root密码或SSH公钥二选一，已发布`.111` `/srv/101cloud/releases/20261104-password-or-public-key`。仅显示/提交所选凭据；公钥模式1–10条、每条最多8192字节，支持标准Ed25519/RSA≥2048/ECDSA并去重，确认页只显示数量。删除登录区域/确认页长说明，保留简短限制和错误提示。秘密仍只存私有会话内存，未知请求冻结原体/幂等键；旧实例不改。前端50文件491条测试、类型检查及构建通过，实际浏览器390px验证公钥模式不要求或显示密码，未提交创建；无SSH工具真实镜像公钥文件及权限验证见backend README。
