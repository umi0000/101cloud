# PVE 192.168.1.99：Ubuntu 26 LXC 使用 NVIDIA 的实测记录

> 历史记录：本页对应主机重装前的 PVE 环境，不代表 `192.168.1.99` 当前状态。主机现在运行 Ubuntu + Incus，当前实例见 [Incus Ubuntu 24.04 单卡配置](INCUS_UBUNTU24_GPU_SETUP.md)。

## 1. 本次只做了什么

用户限定范围：通过 `ssh root@192.168.1.99` 配置 NVIDIA 的 LXC 使用能力，创建一个 Ubuntu 26 容器，验证并记录后续开发要注意的问题。

实际变更：

- 新建 **CT 101 / `ubuntu26-nvidia`** 及其 16 GiB rootfs。
- 只在 CT 101 配置 NVIDIA 计算设备、Ubuntu 26 必需的 `nesting=1` 兼容选项和匹配驱动的用户态文件。
- 容器使用现有 `vmbr0`、DHCP；没有修改宿主机网桥、网口、路由、防火墙或管理网。
- **没有升级/重装宿主机 NVIDIA 驱动，没有升级内核，没有重启宿主机。**
- **没有对已有 CT 100 执行配置或启停操作。** 最终只读列表中 CT 100 仍在运行。
- 没有安装平台后端、Agent、Docker、PyTorch、完整 CUDA Toolkit 或宿主机新软件包。
- 在 CT 101 执行了一次 `apt-get update` 检查候选版本；没有安装仓库中的 NVIDIA 包，也没有执行系统升级。

本文记录实际观察结果，不把尚未安装的框架或尚未完成的压力测试写成已验证。

## 2. 实际环境，不再按之前的假设写

| 项目 | 实测结果 |
| --- | --- |
| PVE 地址/主机名 | `192.168.1.99` / `pve` |
| PVE 版本 | `pve-manager/9.2.21/4f6e0ac86f9e8c7f` |
| 正在运行的内核 | `7.0.14-25-pve` |
| NVIDIA 驱动 | `580.196.07` |
| 显卡 | **4 张 NVIDIA TITAN X (Pascal)，不是 TITAN Xp** |
| 每卡显存 | NVIDIA 报告 `12288 MiB` |
| CUDA 计算能力 | 容器内实测 `6.1` |
| 系统模板 | `local:vztmpl/ubuntu-26.04-standard_26.04-1_amd64.tar.zst`，创建前已经缓存 |
| rootfs 存储 | 现有 `local-lvm` |
| 业务网络 | 已有 `vmbr0`，宿主机 `192.168.1.99/24`，默认网关 `192.168.1.1` |

## 3. 创建后的容器

| 项目 | 最终配置 |
| --- | --- |
| ID / 名称 | `101` / `ubuntu26-nvidia` |
| 系统 | Ubuntu 26.04 PVE 标准模板 |
| 隔离 | `unprivileged: 1`；没有设置 AppArmor unconfined |
| 兼容选项 | `features: nesting=1`，原因见下一节 |
| CPU / RAM / Swap | 2 核 / 4096 MiB / 0 |
| rootfs | `local-lvm:vm-101-disk-0,size=16G` |
| 网卡 | `eth0` → `vmbr0`，IPv4 DHCP，`ip6=manual` |
| 当前 IPv4 | `192.168.1.31/24`，**DHCP 地址，不保证永久不变** |
| 开机自启 | `onboot: 0`；当前容器保持运行 |
| PVE 控制台 | `cmode: shell` |
| root 密码 | `passwd -S root` 显示 `L`（锁定）；没有设置公开密码或追加 SSH 公钥 |
| 访问方式 | PVE 已认证控制台，或在宿主机执行 `pct enter 101` |

不能把 `192.168.1.31` 当作已经配置好 root SSH 登录：本次没有设置容器 SSH 登录凭据。

最终 GPU 配置如下，**不是 `/dev/nvidia0`**：

```text
dev0: path=/dev/nvidia3,mode=0660
dev1: path=/dev/nvidiactl,mode=0660
dev2: path=/dev/nvidia-uvm,mode=0660
```

只提供指定一张物理卡和必要控制设备，没有额外提供其他 GPU、`/dev/dri`、MIG capability 设备或 GPU profiling 设备。

## 4. 第一个坑：Ubuntu 26 的 systemd 259

创建时 PVE 给出：

```text
WARN: Systemd 259 detected. You may need to enable nesting.
```

这次不是只看到警告就关闭安全限制，而是先按默认非特权配置启动检查。实际观察到：

- 容器虽然显示 `running`，但网卡 `eth0` 为 `DOWN`。
- `systemd-networkd` 以 `243/CREDENTIALS` 退出。
- 日志、网络、解析等共 19 个 systemd unit 失败。

只针对新建 CT 101 开启 PVE `nesting=1`，停止再启动该容器后：

- `systemctl is-system-running --wait` 返回 `running`。
- 最终 `systemctl --failed` 为 0。
- DHCP 正常取得 `192.168.1.31`。

**后续开发注意：**

- `PVE 状态 running` 不足以证明镜像正常启动，应检查必要服务和客体网络。
- 当前这套 Ubuntu 26 / systemd 259 / PVE 组合需要把这个兼容选项纳入验证过的模板配置，而不是每个实例出问题后临时手修。
- `nesting=1` 会增加部分 `/proc`、`/sys` 信息的可见性；它不是“完全没有安全影响”。这里仍是非特权容器，没有关闭宿主机或容器的 AppArmor。
- 这是本次环境的实测结论，不据此声称所有 PVE/Ubuntu 版本组合都需要该选项。

## 5. 第二个坑：宿主机序号不等于设备文件编号

本次启动周期的实际映射：

| 宿主机 `nvidia-smi` 序号 | PCI 地址 | UUID | 设备文件 |
| --- | --- | --- | --- |
| 0 | `00000000:05:00.0` | `GPU-7f665bf9-5911-4f71-ac6a-a66e32362574` | `/dev/nvidia3` |
| 1 | `00000000:06:00.0` | `GPU-f5f4cd05-de43-485a-10ef-8e952f3d292b` | `/dev/nvidia2` |
| 2 | `00000000:09:00.0` | `GPU-25f18cf4-ac98-cd0e-20bb-9a353f44d65f` | `/dev/nvidia1` |
| 3 | `00000000:0A:00.0` | `GPU-6d760912-277a-68f0-5d71-d5ae6ac67711` | `/dev/nvidia0` |

最初按 `/dev/nvidia0` 配置时，容器实际看到的是宿主机第 3 张卡的 UUID。核对后已在停止状态下改为 `/dev/nvidia3`，最终目标与描述一致。

**后续选卡不能写成 `'/dev/nvidia' + 网页序号`。** 正确流程：

1. 用 GPU UUID 保存资源身份。
2. 每次配置前查当前 UUID 对应的设备次编号，再生成设备路径。
3. 容器启动后回读 CUDA/NVML UUID，确认确实是选中的卡。

本次用 `nvidia-smi -q -x` 的 XML 读取 `minor_number`。这台驱动的 `nvidia-smi --query-gpu=minor_number` 返回“不支持该字段”，不能把这个查询字段直接写死。正式 Agent 可以使用 NVML 的相应设备查询接口，仍需核对版本。

容器内仅有一张卡时，其显示序号又会变成 `GPU 0`，所以容器内序号也不能当成宿主机序号。宿主机重启/驱动重载后的映射尚未测试，必须重新发现，不保证沿用本表。

## 6. 第三个坑：Ubuntu 仓库的用户态版本与宿主机不一致

实测版本：

- 宿主机驱动：`580.196.07`。
- Ubuntu 26 仓库的 `nvidia-utils-580`、`libnvidia-compute-580` 候选：`580.178.04-0ubuntu0.26.04.1`。

**同属 580 分支也不能直接当成完全匹配。** 为避免 driver/library mismatch，本次没有安装这些候选包，也没有为了迁就它们升级宿主机。没有故意安装错误版本去制造故障。

使用 `pct push` 将宿主机已有的以下同版本用户态文件复制到新容器：

| 宿主机文件 | 容器目标 |
| --- | --- |
| `/usr/bin/nvidia-smi` | `/usr/local/bin/nvidia-smi` |
| `/usr/lib/x86_64-linux-gnu/libcuda.so.580.196.07` | `/usr/local/lib/libcuda.so.580.196.07` |
| `/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.580.196.07` | `/usr/local/lib/libnvidia-ml.so.580.196.07` |
| `/usr/lib/x86_64-linux-gnu/libnvidia-ptxjitcompiler.so.580.196.07` | `/usr/local/lib/libnvidia-ptxjitcompiler.so.580.196.07` |
| `/usr/lib/x86_64-linux-gnu/libnvidia-nvvm.so.580.196.07` | `/usr/local/lib/libnvidia-nvvm.so.580.196.07` |
| `/usr/lib/x86_64-linux-gnu/libnvidia-gpucomp.so.580.196.07` | `/usr/local/lib/libnvidia-gpucomp.so.580.196.07` |

然后只在容器里执行 `ldconfig`，生成对应 SONAME 链接和缓存。本次是**复制文件**，不是把宿主机库目录可写挂载给容器。

另外创建了容器内链接：

```text
/usr/bin/nvidia-smi -> /usr/local/bin/nvidia-smi
```

原因是 `pct exec` 实测 PATH 为 `/sbin:/bin:/usr/sbin:/usr/bin`，没有 `/usr/local/bin`。文件已复制但直接执行 `pct exec 101 -- nvidia-smi` 仍可能报告找不到命令；这个链接解决了当前调用方式的问题。

**后续开发注意：**

- 容器使用宿主机内核驱动，不应在里面安装 DKMS、内核模块或为了容器执行宿主机驱动升级。
- 当前 GPU 用户态文件不是通过容器的 apt 管理。以后宿主机驱动升级，需要同步镜像/容器内的相关版本，不能只改宿主机。
- 宿主机 `dpkg-query -S /usr/bin/nvidia-smi` 未找到所属包。没有核实其完整安装流程，不能假定现有宿主机驱动完全由 apt 管理。
- 这套复制方式已经验证 CUDA 计算和 NVML，不等于已安装完整 CUDA Toolkit、图形栈、视频编解码栈或所有调试工具。
- 容器备份会带上复制进去的文件，但恢复到不同驱动或不同 GPU 环境后仍要重新核对，不能把备份成功等同于 GPU 可用。

## 7. 实际验证，不只看显卡列表

### 7.1 单卡身份与 CUDA 计算

通过容器内自带 Python 的 `ctypes` 调用真实 CUDA Driver API。测试脚本经标准输入执行，没有安装 CUDA Toolkit 或额外框架，也没有留下测试脚本。

实测：

- CUDA 可访问设备数为 **1**。
- GPU UUID 与目标 `GPU-7f665bf9-5911-4f71-ac6a-a66e32362574` 一致。
- 计算能力为 `6.1`。
- 分配 4096 字节 GPU 内存，初始化 1024 个整数为 123。
- 加载 `.target sm_61` 的 PTX，在 GPU 上执行加 7 内核。
- 同步后读回，**1024 个结果全部为 130**。
- 请求 CUDA ordinal 1 返回 `CUDA_ERROR_INVALID_DEVICE`（101），没有第二张可用卡。
- 退出时卸载模块、释放显存并销毁 CUDA 上下文。

驱动 API 版本返回 `13000`。这不代表已安装 CUDA 13 Toolkit，更不能据此声称 CUDA 13 的编译器或最新版 PyTorch 支持 Pascal；应用仍需选支持 `sm_61` 的构建。

### 7.2 简单防绕过检查

在 CT 101 内，以容器 root 尝试为其他三个 NVIDIA 次编号创建临时字符设备：全部在 `mknod` 时返回 `EPERM`。临时路径已清理，没有改变容器的设备允许列表。

最终 `dev[n]` 配置要求 `mode=0660`，但最终运行态 `stat` 观察到设备节点权限为 `0666`。因此**不能把设备 mode 字段当成卡数隔离或计费安全边界**；容器 root 也可能改变自己可管理节点的文件权限。当前单卡访问结果依赖于 PVE/LXC 实际设备授权及隔离，已验证其他 CUDA ordinal 不可用和上述简单 mknod 绕过被拒绝。这不是完整安全审计。

## 8. 对后续平台开发的直接要求

1. GPU 身份必须保存 UUID，启动前重新查询 UUID → 设备次编号；启动后再核对 UUID。
2. 首先建立实测兼容矩阵：PVE、内核、宿主机驱动、Ubuntu 模板、systemd、容器库和应用框架版本。
3. PVE `dev[n]` 在当前环境可以为非特权 LXC 提供计算设备，本次不需要手写 `lxc.cgroup2.devices.allow` 或开放全部 NVIDIA 设备。
4. 不要硬编码 UVM 设备主编号；本次 `/dev/nvidia-uvm` 的主编号为 234，设备路径和当前实际节点才是配置输入。
5. 新实例/克隆不能直接沿用模板里某个用户的 `/dev/nvidia3` 绑定；应由平台重新选择并注入。
6. 当前 `onboot=0`。未来若平台管理 GPU 启动，应先校验设备映射和授权，不让宿主机自动启动旁路绕开分配逻辑。
7. root 密码锁定与 GPU 不可用是不同问题。平台应通过已鉴权 PVE 控制台提供访问，或在用户明确要求后配置 SSH 登录，不能为了方便设置公共默认密码。
8. IP `192.168.1.31` 来自 DHCP，不是平台 IPAM 已实现。当前没有为这个容器改 DHCP 服务器、静态地址池或 NAT。
9. 后续 CPU/GPU 卡时仍需宿主机 Agent 与业务账本；本次没有安装或验证计量系统。

## 9. 没做、也不能声称已经验证的事情

- 没有升级宿主机驱动、重启物理机或验证驱动重载后的编号稳定性。
- 没有修改 CT 100，也没有另外创建第二个 GPU 容器；跨容器共享压力测试没有执行。
- 没有安装 PyTorch、TensorFlow、ComfyUI 或编译器，不能保证它们任意版本直接可用。
- 没有测试四卡任务、GPU 抢占、运行中换卡、显存硬配额或按量计费。
- 没有改管理网、业务网拓扑、防火墙、路由、端口映射或业务平台代码。

**当前交付事实：CT 101 是运行中的 Ubuntu 26.04 非特权 LXC，已能使用指定的一张 TITAN X (Pascal) 执行真实 CUDA 计算。**
