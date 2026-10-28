# PVE LXC 使用宿主机 NVIDIA 显卡教程

> 环境已变更：`192.168.1.99` 已由用户重装为 Ubuntu，当前使用 Incus。本教程保留旧 PVE 配置方法，不应在当前主机上继续执行 PVE 命令。当前容器和实测结果见 [Incus Ubuntu 24.04 单卡配置](INCUS_UBUNTU24_GPU_SETUP.md)。

## 1. 这份教程适用什么情况？

目标：在**宿主机 NVIDIA 驱动已经正常工作**的前提下，让一个 Ubuntu 26.04 非特权 LXC 使用指定的一张显卡。

这不是把显卡从宿主机拿走交给 VM，而是让容器里的程序通过受控设备访问宿主机驱动。其他容器也可以使用同一张卡，但不会因此自动获得显存硬配额。

下面的做法来自 `192.168.1.99` 的实际配置：PVE 9.2.21、内核 `7.0.14-25-pve`、驱动 `580.196.07`、TITAN X (Pascal)、Ubuntu 26.04。详见 [本次实测记录](PVE_GPU_SETUP_NOTES.md)。换系统、驱动或显卡后，需要重新验证，不能保证原样通用。

旧 PVE 试验使用的是 CT 101。本文创建步骤只适用于仍运行 PVE 的环境，会选择空闲 ID；不要把旧记录的 CT 101 当成当前 Incus 容器，当前实例名称为 `ubuntu24-gpu`。

所有操作命令都在 **PVE 宿主机的同一个 Bash 会话**执行；标有 `pct exec` 的命令才是在容器里执行。不同代码块会复用变量，不要中途换终端。

本教程不需要改宿主机网桥、防火墙、路由，不需要重装驱动，也不需要重启宿主机。

## 2. 先确认宿主机正常

连接 PVE：

```bash
ssh root@192.168.1.99
```

检查现状：

```bash
pveversion
uname -r
nvidia-smi -L
pvesm status
pct list
pveam list local
ip -br address
```

需要确认：

- `nvidia-smi` 正常列出显卡。如果宿主机自己都用不了显卡，先解决宿主机问题，不要在容器里乱装驱动。
- `local-lvm` 有足够空间；如果实际存储名称不同，修改下面的存储变量。
- 已有业务网桥，例如 `vmbr0`，且能提供正常的容器网络。
- 模板名称存在，或可以从 PVE 模板库下载。

在同一个会话中启用出错即停，避免前一步失败后继续创建：

```bash
set -euo pipefail
```

这是分步操作教程，不建议未经核对就整篇当成一键脚本执行。

## 3. 按 UUID 选卡，不能按序号拼设备路径

先从 `nvidia-smi -L` 选定一张卡的 UUID。下面使用本机的一张卡作为例子；在其他机器上必须替换为其真实 UUID。

```bash
export GPU_UUID='GPU-7f665bf9-5911-4f71-ac6a-a66e32362574'

GPU_DEV="$(python3 - <<'PY'
import os
import stat
import subprocess
import xml.etree.ElementTree as ET

wanted = os.environ['GPU_UUID']
root = ET.fromstring(subprocess.check_output(['nvidia-smi', '-q', '-x']))
matches = [gpu for gpu in root.findall('gpu') if gpu.findtext('uuid') == wanted]
if len(matches) != 1:
    raise SystemExit('找不到唯一匹配的 GPU UUID，停止配置')
minor = int(matches[0].findtext('minor_number'))
if minor < 0:
    raise SystemExit('无效的 NVIDIA 设备次编号')
path = f'/dev/nvidia{minor}'
if not stat.S_ISCHR(os.stat(path).st_mode):
    raise SystemExit(f'{path} 不是字符设备')
print(path)
PY
)"
export GPU_DEV
printf '选择的卡：%s\n对应设备：%s\n' "$GPU_UUID" "$GPU_DEV"

test -c /dev/nvidiactl
test -c /dev/nvidia-uvm
```

这台机器实测：宿主机显示的 **GPU 0 对应 `/dev/nvidia3`**。因此不能认为 `nvidia-smi` 的序号和 `/dev/nvidiaN` 的 N 一定相同。

这里读取 XML 的 `minor_number`，没有使用 `--query-gpu=minor_number`，因为当前驱动不接受后者。

如果控制设备或 UVM 设备不存在，先停下检查宿主机驱动与已有模块；本教程不会自动安装驱动、改 udev 规则或升级内核。

## 4. 准备匹配版本的用户态文件

容器不需要安装 NVIDIA 内核模块，但需要能与宿主机驱动配合的 CUDA/NVML 用户态库。

当前 Ubuntu 26 仓库的 NVIDIA 580 包版本与宿主机不一致，所以采用**复制宿主机同版本文件**的方式，不在容器里安装另一套 `nvidia-driver` 或 DKMS。

先检查文件，全部存在再往下执行：

```bash
DRIVER_VERSION="$(nvidia-smi -i "$GPU_UUID" \
  --query-gpu=driver_version --format=csv,noheader,nounits)"
HOST_NVIDIA_SMI="$(command -v nvidia-smi)"
HOST_LIBDIR=/usr/lib/x86_64-linux-gnu
export DRIVER_VERSION

printf '使用宿主机驱动版本：%s\n' "$DRIVER_VERSION"
test -f "$HOST_NVIDIA_SMI"

for name in cuda nvidia-ml nvidia-ptxjitcompiler nvidia-nvvm nvidia-gpucomp; do
  test -f "$HOST_LIBDIR/lib${name}.so.${DRIVER_VERSION}"
done
```

这组文件名和目录针对本次实测的 x86_64 / 580 驱动环境。其他驱动如果文件布局不同，应先确认实际依赖，**不要通过混入其他版本库来凑齐**。

复制方式的取舍是：容器内这些文件不由 apt 管理。以后升级宿主机驱动，要同步更新相关镜像和容器，不能只升级宿主机。换成包管理方案前也应明确迁移这些文件，避免两套版本混用。

## 5. 创建新的 Ubuntu 26.04 容器

### 5.1 设置模板、网桥和存储

```bash
TEMPLATE=ubuntu-26.04-standard_26.04-1_amd64.tar.zst
TEMPLATE_STORAGE=local
ROOTFS_STORAGE=local-lvm
BRIDGE=vmbr0

TEMPLATE_PATH="$(pvesm path "${TEMPLATE_STORAGE}:vztmpl/${TEMPLATE}")"
if [ ! -f "$TEMPLATE_PATH" ]; then
  pveam download "$TEMPLATE_STORAGE" "$TEMPLATE"
fi
```

下载仅用于模板没有缓存时；已有模板不会重新下载。若 PVE 当前模板清单中没有 Ubuntu 26，先核实模板清单，不要换成不明来源 rootfs。

### 5.2 获取空闲 ID，并避免碰现有实例

```bash
export CTID="$(pvesh get /cluster/nextid --output-format json \
  | python3 -c 'import json,sys; print(int(json.load(sys.stdin)))')"

pvesh get /cluster/resources --type vm --output-format json \
  | python3 -c 'import json,os,sys; ct=int(os.environ["CTID"]); rows=json.load(sys.stdin); assert not any(r.get("vmid")==ct for r in rows), "ID 已占用，停止"'

printf '准备创建新容器，ID=%s\n' "$CTID"
```

`nextid` 只是查询，不是预留。若其他管理员同时创建导致冲突，应重新选择 ID，不能删除或覆盖对方的实例。

### 5.3 创建非特权 LXC，并只开放选中的卡

```bash
pct create "$CTID" "${TEMPLATE_STORAGE}:vztmpl/${TEMPLATE}" \
  --hostname "ubuntu26-nvidia-${CTID}" \
  --cores 2 \
  --memory 4096 \
  --swap 0 \
  --rootfs "${ROOTFS_STORAGE}:16" \
  --unprivileged 1 \
  --features nesting=1 \
  --onboot 0 \
  --cmode shell \
  --net0 "name=eth0,bridge=${BRIDGE},ip=dhcp,ip6=manual,type=veth" \
  --dev0 "path=${GPU_DEV},mode=0660" \
  --dev1 path=/dev/nvidiactl,mode=0660 \
  --dev2 path=/dev/nvidia-uvm,mode=0660 \
  --description "Ubuntu 26.04 NVIDIA CUDA; ${GPU_UUID} only"
```

几个选项为什么这样设：

- `unprivileged=1`：保持非特权容器，不靠把容器变成特权模式解决 GPU 问题。
- `nesting=1`：当前实测的 Ubuntu 26 / systemd 259 环境需要它，否则网络、日志等服务出现 `243/CREDENTIALS`。它增加部分 `/proc`、`/sys` 信息可见性，不是没有安全影响；不要因此再关闭 AppArmor。
- `dev0/dev1/dev2`：一个实际 GPU 节点，加 CUDA 必需控制设备。这里只使用新容器的空闲设备键，没有开放全部 GPU。
- `onboot=0`：先不自动启动。宿主机重启后应重新核对 UUID 与设备编号，不应盲目相信旧映射。
- `cmode=shell`：已通过 PVE 鉴权的控制台可以进入容器，不设置公共默认 root 密码。
- 网络只接现有网桥、使用 DHCP，不修改宿主机网络。

**如果配置的是已有容器，不要执行这条创建命令。** 先查看其 `pct config`，在允许停机后配置；保留已有 `features` 和其他设备，不要直接用 `dev0` 覆盖 USB、磁盘或其他设备。已有 GPU 绑定也要逐项核对，避免新加一张后原来的卡仍可访问。

## 6. 启动容器，安装用户态文件

启动并等待 systemd 初始化完成：

```bash
pct start "$CTID"
pct exec "$CTID" -- systemctl is-system-running --wait
pct exec "$CTID" -- systemctl --failed --no-pager
pct exec "$CTID" -- ip -br address
```

若存在失败服务，先看具体原因，不要把“PVE 显示 running”当成系统已经正常。

复制用户态文件，所有目标都在**新容器内部**：

```bash
pct push "$CTID" "$HOST_NVIDIA_SMI" \
  /usr/local/bin/nvidia-smi --perms 0755

for name in cuda nvidia-ml nvidia-ptxjitcompiler nvidia-nvvm nvidia-gpucomp; do
  pct push "$CTID" \
    "$HOST_LIBDIR/lib${name}.so.${DRIVER_VERSION}" \
    "/usr/local/lib/lib${name}.so.${DRIVER_VERSION}" \
    --perms 0644
done

pct exec "$CTID" -- ldconfig
pct exec "$CTID" -- /usr/local/bin/nvidia-smi -L
```

当前 Ubuntu 模板会让 `ldconfig` 处理 `/usr/local/lib`，并生成 `.so.1` 等 SONAME 链接。最后应该只列出选中的 UUID。容器中可能显示为 `GPU 0`，要比较 **UUID**，不能比较序号。

`pct exec` 的 PATH 在当前环境不包含 `/usr/local/bin`。如果希望直接运行 `nvidia-smi`，可在这个尚未安装其他 NVIDIA 工具的新容器中创建链接：

```bash
pct exec "$CTID" -- test ! -e /usr/bin/nvidia-smi
pct exec "$CTID" -- ln -s /usr/local/bin/nvidia-smi /usr/bin/nvidia-smi
pct exec "$CTID" -- nvidia-smi -L
```

这里没有使用 `ln -sf`。如果目标已经存在，应先核对它的来源与版本，不要覆盖不明文件。

## 7. 验证真实 CUDA 计算，不只看显卡列表

下面脚本在容器中使用自带 Python，调用 CUDA Driver API，分配显存并执行一个小 GPU 内核。无需安装 PyTorch、编译器或完整 CUDA Toolkit。

脚本针对当前 Pascal / `sm_61` 单卡环境；其他 GPU 不应盲目照搬目标架构。先确认前面执行的 `export CTID` 和 `export GPU_UUID` 仍在当前会话有效。

```bash
pct exec "$CTID" -- env EXPECTED_GPU_UUID="$GPU_UUID" python3 - <<'PY'
import ctypes as C
import json
import os
import uuid

cuda = C.CDLL('libcuda.so.1')
P, U, I, D = C.c_void_p, C.c_uint, C.c_int, C.c_uint64
signatures = {
    'cuInit': [U],
    'cuDeviceGetCount': [C.POINTER(I)],
    'cuDeviceGet': [C.POINTER(I), I],
    'cuDeviceGetUuid': [P, I],
    'cuCtxCreate_v2': [C.POINTER(P), U, I],
    'cuCtxDestroy_v2': [P],
    'cuMemAlloc_v2': [C.POINTER(D), C.c_size_t],
    'cuMemFree_v2': [D],
    'cuMemsetD32_v2': [D, U, C.c_size_t],
    'cuMemcpyDtoH_v2': [P, D, C.c_size_t],
    'cuModuleLoadData': [C.POINTER(P), P],
    'cuModuleGetFunction': [C.POINTER(P), P, C.c_char_p],
    'cuModuleUnload': [P],
    'cuLaunchKernel': [P] + [U] * 7 + [P, C.POINTER(P), C.POINTER(P)],
    'cuCtxSynchronize': [],
    'cuGetErrorName': [I, C.POINTER(C.c_char_p)],
}
for symbol, types in signatures.items():
    getattr(cuda, symbol).argtypes = types
    getattr(cuda, symbol).restype = I

def call(symbol, *args):
    code = getattr(cuda, symbol)(*args)
    if code:
        label = C.c_char_p()
        cuda.cuGetErrorName(code, C.byref(label))
        raise RuntimeError(f'{symbol}: {code} {label.value!r}')

call('cuInit', 0)
count, device = I(), I()
call('cuDeviceGetCount', C.byref(count))
assert count.value == 1, f'预期只有一张可用卡，实际 {count.value}'
call('cuDeviceGet', C.byref(device), 0)
uuid_bytes = (C.c_ubyte * 16)()
call('cuDeviceGetUuid', uuid_bytes, device)
actual_uuid = 'GPU-' + str(uuid.UUID(bytes=bytes(uuid_bytes)))
assert actual_uuid == os.environ['EXPECTED_GPU_UUID'], actual_uuid
assert cuda.cuDeviceGet(C.byref(I()), 1) == 101, '发现了第二张可用卡'

ptx = b'''.version 7.0
.target sm_61
.address_size 64
.visible .entry add_seven(.param .u64 output, .param .u32 count) {
    .reg .pred %p;
    .reg .b32 %r<7>;
    .reg .b64 %rd<4>;
    ld.param.u64 %rd0, [output];
    ld.param.u32 %r0, [count];
    mov.u32 %r1, %tid.x;
    mov.u32 %r2, %ctaid.x;
    mov.u32 %r3, %ntid.x;
    mad.lo.s32 %r4, %r2, %r3, %r1;
    setp.ge.u32 %p, %r4, %r0;
    @%p bra DONE;
    mul.wide.u32 %rd1, %r4, 4;
    add.s64 %rd2, %rd0, %rd1;
    ld.global.u32 %r5, [%rd2];
    add.u32 %r6, %r5, 7;
    st.global.u32 [%rd2], %r6;
DONE:
    ret;
}
'''
context, module, memory = P(), P(), D()
try:
    call('cuCtxCreate_v2', C.byref(context), 0, device)
    n = 1024
    call('cuMemAlloc_v2', C.byref(memory), n * 4)
    call('cuMemsetD32_v2', memory, 123, n)
    ptx_buffer = C.create_string_buffer(ptx)
    call('cuModuleLoadData', C.byref(module), C.cast(ptx_buffer, P))
    kernel = P()
    call('cuModuleGetFunction', C.byref(kernel), module, b'add_seven')
    length = U(n)
    params = (P * 2)(C.cast(C.byref(memory), P), C.cast(C.byref(length), P))
    call('cuLaunchKernel', kernel, 4, 1, 1, 256, 1, 1, 0, None, params, None)
    call('cuCtxSynchronize')
    output = (U * n)()
    call('cuMemcpyDtoH_v2', output, memory, n * 4)
    assert all(value == 130 for value in output), 'GPU 计算结果错误'
finally:
    if module.value:
        call('cuModuleUnload', module)
    if memory.value:
        call('cuMemFree_v2', memory)
    if context.value:
        call('cuCtxDestroy_v2', context)

print(json.dumps({
    'status': 'PASS', 'gpu_uuid': actual_uuid,
    'visible_gpu_count': count.value,
    'verified_elements': n, 'result_each': 130,
}))
PY
```

正确结果应包含 `status: PASS`、指定 UUID、单卡可见，以及 1024 个结果均为 130。执行结束会释放显存与上下文，不会保存一个持续运行的测试进程。

这证明当前驱动路径能执行 CUDA，不证明任意版本 PyTorch、TensorFlow、ComfyUI 都兼容 Pascal。

## 8. 常见问题怎么排查？

| 现象 | 先查什么 | 不要怎么做 |
| --- | --- | --- |
| 宿主机能用，容器看不到卡 | `pct config` 中设备路径、宿主机 UUID→次编号、容器设备是否存在 | 不要先改成特权容器或开放全部设备 |
| `nvidia-smi` 找不到命令 | 文件是否在 `/usr/local/bin`，`pct exec` 的 PATH 是否包含它 | 不要误以为必须重装宿主机驱动 |
| driver/library mismatch | 宿主机驱动与容器用户态文件的具体版本 | 不要因为分支都叫 580 就认为匹配 |
| systemd 服务失败、网卡 DOWN、`243/CREDENTIALS` | 当前 Ubuntu 26 模板是否使用已经验证的 `nesting=1` | 不要关闭 AppArmor 或全局安全设置来掩盖问题 |
| `nvidia-smi` 正常但框架失败 | 框架是否支持 Python 版本、CUDA 构建、Pascal `sm_61` | 不要把显示的 CUDA Version 当成已安装的 Toolkit 版本 |
| 另一张卡出现在容器里 | 重新按 UUID 查设备次编号，停止后修正该容器设备配置 | 不要认为宿主机 GPU 0 必然是 `/dev/nvidia0` |
| GPU 提示显存不足 | 同卡其他进程和显存占用 | 不要认为共享人数限制已经把显存平分 |

`mode=0660` 也不是完整的隔离证明。本次配置后运行态曾观察到节点权限为 `0666`。应验证未授权卡无法用于实际计算，不能只看设备文件权限或环境变量。容器 root 也不能获得宿主机控制接口。

## 9. 后续升级、克隆和重启的注意事项

- **升级宿主机驱动后**：同步容器用户态文件，重新运行身份和 CUDA 验证，不能只升级宿主机。
- **宿主机重启或驱动重载后**：重新发现 UUID → 设备次编号，确认旧设备配置仍指向同一张卡。
- **克隆 GPU 模板时**：不要把模板里固定的物理卡授权直接当成新用户的授权；由平台重新选择和注入。
- **配置已有容器时**：先确认停机许可，保存当前配置，保留原有设备与功能选项；不能盲目覆盖 `dev0` 或整个 `features`。
- **普通停止/启动**：只在确认需要停这个容器时执行 `pct stop "$CTID"`、`pct start "$CTID"`；停止会中断其中的服务和计算。
- **备份恢复后**：虽然用户态文件包含在容器磁盘里，目标宿主机仍需匹配的驱动和正确设备映射。
- **访问容器**：PVE 控制台或 `pct enter "$CTID"`；DHCP 地址不等于已经配置了 SSH 凭据。

**总结：宿主机驱动正常 → 按 UUID 找到设备 → 给非特权 LXC 配指定设备 → 放入匹配的用户态库 → 实测 CUDA。不要在容器里重新安装一套内核驱动。**
