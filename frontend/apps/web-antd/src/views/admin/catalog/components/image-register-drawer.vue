<script lang="ts" setup>
import type { Image, ImageNodeInput, ImageSource, ImageSourceID, InstanceType, Node, RemoteImage } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Alert, Button, Drawer, Form, FormItem, Input, message, RadioButton, RadioGroup, Select, Space, Spin, Switch, Tag } from 'ant-design-vue';

import { catalogApi, listAll, nodesApi } from '#/api/backend';
import { revisionTag } from '#/api/request';
import { errorToast, trackAccepted, useAction } from '#/composables/api';
import { describeError } from '#/locales/errors';
import { formatBytes, shortId } from '#/utils/format';

import { CODE_PATTERN, SHA256_PATTERN } from './rules';

const props = defineProps<{ image?: Image | null; open: boolean }>();
const emit = defineEmits<{ changed: []; 'update:open': [value: boolean] }>();
const action = useAction();
const nodes = ref<Node[]>([]);
const sources = ref<ImageSource[]>([]);
const source = ref<ImageSourceID>('images');
const mode = ref<'local' | 'remote'>('remote');
const discoveryNode = ref<string>();
const catalog = ref<RemoteImage[]>([]);
const catalogLoading = ref(false);
const catalogError = ref('');
const setupLoading = ref(false);
const setupError = ref('');
const code = ref('');
const version = ref('');
const os = ref('');
const release = ref('');
const type = ref<InstanceType>('container');
const selected = ref<string[]>([]);
interface NodeRow extends ImageNodeInput {
  catalog: RemoteImage[];
  catalogError: string;
  loading: boolean;
  requestSeq: number;
}
const rows = ref<NodeRow[]>([]);
let session = 0;
let catalogSeq = 0;
const architectureCache = new Map<string, string>();
const options = (values: string[]) => [...new Set(values)].toSorted().map((value) => ({ label: value, value }));
const osOptions = computed(() => options(catalog.value.map((i) => i.os_name)));
const releaseOptions = computed(() => options(catalog.value.filter((i) => i.os_name === os.value).map((i) => i.os_release)));
const typeOptions = computed(() => {
  const available = new Set(catalog.value.filter((i) => i.os_name === os.value && i.os_release === release.value).map((i) => i.instance_type));
  return [{ label: '容器', value: 'container' }, { label: '虚拟机', value: 'virtual-machine' }].map((o) => ({ ...o, disabled: mode.value === 'remote' && !available.has(o.value as InstanceType) }));
});
const nodeOptions = computed(() => nodes.value.map((n) => ({ label: `${n.name}${n.health === 'online' ? '' : '（未在线）'}`, value: n.id })));
const discoveryOptions = computed(() => nodes.value.map((n) => ({ disabled: n.health !== 'online', label: `${n.name}${n.health === 'online' ? '' : '（未在线）'}`, value: n.id })));
const sourceOptions = computed(() => sources.value.map((s) => ({ disabled: s.available === false, label: `${s.name}${s.available === false ? `（${s.unsupported_reason || '暂不支持'}）` : ''}`, value: s.id })));
const selectedSource = computed(() => sources.value.find((s) => s.id === source.value));
function nodeName(id: string) { return nodes.value.find((n) => n.id === id)?.name ?? `节点 #${id}`; }
function rowImages(row: NodeRow) {
  return row.catalog.filter((i) => i.os_name === os.value && i.os_release === release.value && i.instance_type === type.value);
}
function architectureOptions(row: NodeRow) {
  return options([row.architecture, ...rowImages(row).map((i) => i.architecture)].filter(Boolean)).map((o) => ({ ...o, disabled: !rowImages(row).some((i) => i.architecture === o.value) }));
}
function cloudOptions(row: NodeRow) {
  const candidates = rowImages(row).filter((i) => i.architecture === row.architecture);
  return [{ label: '带 cloud-init', value: 'cloud' }, { label: '不带 cloud-init', value: 'plain' }].map((o) => ({ ...o, disabled: !candidates.some((i) => i.cloud_init === (o.value === 'cloud')) }));
}
function selectCloud(row: NodeRow, value: unknown) {
  if (value !== 'cloud' && value !== 'plain') return;
  row.cloud_init = value === 'cloud';
  synchronizeArtifact(row);
}
function artifacts(row: NodeRow) {
  return rowImages(row).filter((i) => i.architecture === row.architecture && i.cloud_init === row.cloud_init);
}
function artifactOptions(row: NodeRow) {
  return artifacts(row).map((i) => ({ label: `${i.description || i.variant} · ${i.variant || 'default'} · ${formatBytes(i.size_bytes)} · ${shortId(i.fingerprint)}`, value: i.fingerprint }));
}
function synchronizeArtifact(row: NodeRow) {
  const candidates = artifacts(row);
  if (!candidates.some((i) => i.fingerprint === row.fingerprint)) row.fingerprint = candidates[0]?.fingerprint ?? '';
  row.variant = candidates.find((i) => i.fingerprint === row.fingerprint)?.variant ?? '';
  action.resetKey();
}

async function loadRow(row: NodeRow) {
  const mine = session;
  const src = source.value;
  const request = ++row.requestSeq;
  row.loading = true;
  row.catalogError = '';
  row.catalog = [];
  try {
    const result = await catalogApi.remoteImages(row.node_id, src);
    if (mine !== session || request !== row.requestSeq || src !== source.value || !rows.value.includes(row) || mode.value !== 'remote') return;
    row.catalog = result.data.items;
    synchronizeArtifact(row);
  } catch (error) {
    if (mine === session && request === row.requestSeq && src === source.value) row.catalogError = describeError(error).text;
  } finally {
    if (mine === session && request === row.requestSeq && src === source.value) row.loading = false;
  }
}

async function loadCatalog() {
  const mine = ++catalogSeq;
  catalog.value = [];
  catalogError.value = '';
  catalogLoading.value = false;
  if (!props.open || mode.value !== 'remote' || !discoveryNode.value) return;
  catalogLoading.value = true;
  try {
    const result = await catalogApi.remoteImages(discoveryNode.value, source.value);
    if (mine !== catalogSeq) return;
    catalog.value = result.data.items;
    if (!props.image && !osOptions.value.some((o) => o.value === os.value)) os.value = '';
    if (result.data.items.length === 0) catalogError.value = '该来源当前未返回可用镜像，请更换来源或重试。';
  } catch (error) {
    if (mine === catalogSeq) catalogError.value = describeError(error).text;
  } finally {
    if (mine === catalogSeq) catalogLoading.value = false;
  }
}

watch(() => props.open, async (open) => {
  ++session;
  ++catalogSeq;
  if (!open) return;
  const mine = session;
  action.resetKey();
  code.value = '';
  version.value = '';
  os.value = props.image?.os_name ?? '';
  release.value = props.image?.os_release ?? '';
  type.value = props.image?.instance_type ?? 'container';
  mode.value = props.image?.source_type ?? 'remote';
  source.value = props.image?.nodes.find((n) => n.source_id)?.source_id ?? 'images';
  selected.value = [];
  rows.value = [];
  catalog.value = [];
  catalogError.value = '';
  setupError.value = '';
  setupLoading.value = true;
  try {
    const [n, s] = await Promise.all([listAll(nodesApi.list), catalogApi.imageSources()]);
    if (mine !== session) return;
    nodes.value = n;
    sources.value = s.data.items;
    discoveryNode.value = n.find((node) => node.health === 'online')?.id;
    await loadCatalog();
  } catch (error) {
    if (mine === session) setupError.value = describeError(error).text;
  } finally {
    if (mine === session) setupLoading.value = false;
  }
});

watch([source, discoveryNode, mode], () => {
  action.resetKey();
  void loadCatalog();
});
watch([source, mode], () => {
  for (const row of rows.value) {
    ++row.requestSeq;
    row.loading = false;
    row.fingerprint = '';
    row.variant = '';
    row.catalog = [];
    row.catalogError = '';
    if (mode.value === 'remote') void loadRow(row);
  }
});
watch(os, () => {
  if (!props.image && !releaseOptions.value.some((o) => o.value === release.value)) release.value = '';
});
watch([os, release, type], () => {
  for (const row of rows.value) if (mode.value === 'remote') synchronizeArtifact(row);
  action.resetKey();
});
watch([rows, code, version], () => action.resetKey(), { deep: true });
async function initializeRow(row: NodeRow) {
  const mine = session;
  if (!row.architecture) {
    try {
      const result = await nodesApi.resources(row.node_id);
      const architecture = result.data.resources?.server_resources?.cpu?.architecture;
      if (mine !== session || !rows.value.includes(row)) return;
      if (architecture) { architectureCache.set(row.node_id, architecture); row.architecture = architecture; }
    } catch (error) { if (mine === session && rows.value.includes(row)) errorToast(error); }
  }
  if (mine === session && rows.value.includes(row) && mode.value === 'remote') void loadRow(row);
}

watch(selected, (ids) => {
  rows.value = rows.value.filter((r) => ids.includes(r.node_id));
  for (const id of ids) {
    if (rows.value.some((r) => r.node_id === id)) continue;
    const node = nodes.value.find((n) => n.id === id);
    if (!node) continue;
    const existing = props.image?.nodes.find((n) => n.node_id === id && n.project_name === node.defaults.project_name);
    const input: NodeRow = {
      architecture: existing?.architecture ?? architectureCache.get(id) ?? '',
      catalog: [], catalogError: '', loading: false, requestSeq: 0,
      cloud_init: existing?.cloud_init ?? true,
      fingerprint: existing?.fingerprint ?? '',
      gpu_ready: existing?.gpu_ready ?? false,
      node_id: id,
      project_name: existing?.project_name ?? node.defaults.project_name,
      properties: existing?.properties,
      variant: existing?.variant ?? '',
    };
    rows.value.push(input);
    const row = rows.value.find((r) => r.node_id === id);
    if (!row) continue;
    void initializeRow(row);
  }
  action.resetKey();
});

async function submit() {
  if ((code.value && !CODE_PATTERN.test(code.value)) || (version.value && !CODE_PATTERN.test(version.value))) return message.warning('代码与版本可留空；填写时为 1–64 位字母、数字、_ . -，以字母或数字开头');
  if (!os.value.trim() || !release.value.trim()) return message.warning('请选择或填写系统名称与版本');
  if (!rows.value.length) return message.warning('请至少选择一个目标节点');
  const inputs: ImageNodeInput[] = [];
  for (const row of rows.value) {
    if (!row.project_name.trim() || !row.architecture.trim()) return message.warning(`${nodeName(row.node_id)}：请填写项目与架构`);
    const fingerprint = row.fingerprint.trim().toLowerCase();
    if (!SHA256_PATTERN.test(fingerprint)) return message.warning(`${nodeName(row.node_id)}：请提供 64 位完整 SHA-256 指纹；可用 incus image info <短指纹> 或 incus image list -c lFd 查看`);
    const existing = props.image?.nodes.find((n) => n.node_id === row.node_id && n.project_name === row.project_name.trim());
    if (existing && ['pending', 'downloading', 'unknown'].includes(existing.status)) return message.warning(`${nodeName(row.node_id)}：当前配置${existing.status === 'unknown' ? '结果未知，请先核对原任务' : '正在准备中，请等待任务结束'}`);
    const artifact = artifacts(row).find((i) => i.fingerprint === fingerprint);
    if (mode.value === 'remote' && (!artifact || row.loading || row.catalogError)) return message.warning(`${nodeName(row.node_id)}：请先从该节点的真实目录选择匹配架构与 cloud-init 的镜像`);
    inputs.push({
      architecture: row.architecture.trim(), cloud_init: row.cloud_init, fingerprint, gpu_ready: row.gpu_ready,
      node_id: row.node_id, project_name: row.project_name.trim(), properties: row.properties,
      ...(mode.value === 'remote' ? { source_id: source.value, variant: artifact?.variant ?? '' } : {}),
    });
  }
  const image = props.image;
  if (image) {
    const response = await action.run((key) => catalogApi.prepareImageNodes(image.id, inputs, { key, ifMatch: revisionTag(image.revision) }), { onConflict: () => emit('changed'), success: '节点准备任务已提交' });
    if (!response) return;
    for (const task of response.data.tasks) trackAccepted({ instance_id: null, status: 'queued', task_id: task.task_id }, `准备镜像 · ${nodeName(task.node_id)}`, () => emit('changed'));
  } else {
    const response = await action.run((key) => catalogApi.createImage({ code: code.value.trim() || undefined, version: version.value.trim() || undefined, instance_type: type.value, source_type: mode.value, properties: { gpu_ready: false, os_name: os.value.trim(), os_release: release.value.trim() }, nodes: inputs }, { key }), { success: '镜像已登记，节点准备任务已提交' });
    if (!response) return;
    for (const node of response.data.nodes) if (node.task_id) trackAccepted({ instance_id: null, status: 'queued', task_id: node.task_id }, `准备镜像 · ${nodeName(node.node_id)}`, () => emit('changed'));
  }
  emit('changed');
  emit('update:open', false);
}
</script>

<template>
  <Drawer :open="open" :title="image ? `配置镜像节点 · ${image.name}` : '登记镜像'" width="min(960px, 100vw)" @close="emit('update:open', false)">
    <Alert class="mb-4" show-icon type="info" message="按节点独立准备镜像。远程镜像自动下载，本地镜像只校验是否已存在；只有校验就绪后，才可在对应节点创建或纳管实例。" />
    <Alert v-if="setupError" class="mb-4" show-icon type="error" :message="setupError" />
    <Spin :spinning="setupLoading">
      <Form layout="vertical" @change="action.resetKey()">
        <FormItem label="镜像来源" required>
          <RadioGroup v-model:value="mode" :disabled="!!image">
            <RadioButton value="remote">远程镜像 · 自动下载</RadioButton>
            <RadioButton value="local">节点已有本地镜像</RadioButton>
          </RadioGroup>
        </FormItem>
        <div v-if="mode === 'remote'" class="grid grid-cols-1 gap-x-4 sm:grid-cols-2">
          <FormItem label="可信镜像源" required :extra="selectedSource?.url"><Select v-model:value="source" :options="sourceOptions" /></FormItem>
          <FormItem label="目录查询节点" required extra="通过此节点读取官方目录；每个目标节点另行确认自身可下载的配置。"><Select v-model:value="discoveryNode" :options="discoveryOptions" placeholder="选择在线节点" /></FormItem>
        </div>
        <Alert v-if="mode === 'remote' && catalogError" class="mb-3" show-icon type="error" :message="catalogError"><template #action><Button size="small" :loading="catalogLoading" @click="loadCatalog">重试查询</Button></template></Alert>
        <div class="grid grid-cols-1 gap-x-4 sm:grid-cols-3">
          <FormItem label="系统名称" required>
            <Select v-if="mode === 'remote'" v-model:value="os" :disabled="!!image" :loading="catalogLoading" :options="osOptions" show-search placeholder="从真实目录选择系统" />
            <Input v-else v-model:value="os" :disabled="!!image" placeholder="如 ubuntu" />
          </FormItem>
          <FormItem label="系统版本" required>
            <Select v-if="mode === 'remote'" v-model:value="release" :disabled="!!image || !os" :loading="catalogLoading" :options="releaseOptions" show-search placeholder="选择发行版本" />
            <Input v-else v-model:value="release" :disabled="!!image" placeholder="如 24.04" />
          </FormItem>
          <FormItem label="实例类型" required><Select v-model:value="type" :disabled="!!image" :options="typeOptions" /></FormItem>
        </div>
        <div v-if="!image" class="grid grid-cols-1 gap-x-4 sm:grid-cols-2">
          <FormItem label="代码（可选）" extra="留空由指纹自动生成"><Input v-model:value="code" class="font-mono" /></FormItem>
          <FormItem label="版本（可选）" extra="留空为 1"><Input v-model:value="version" class="font-mono" /></FormItem>
        </div>
        <FormItem label="批量选择目标节点" required extra="各节点可以使用不同架构、项目、cloud-init 变体与 GPU 就绪设置。已有未知任务请在节点状态中核对，不要重新下发。">
          <Select v-model:value="selected" mode="multiple" :options="nodeOptions" placeholder="选择一个或多个节点" />
          <Space class="mt-2" wrap><Button size="small" @click="selected = nodes.filter((n) => n.health === 'online').map((n) => n.id)">选择全部在线节点</Button><Button size="small" @click="selected = []">清空选择</Button></Space>
        </FormItem>
        <section v-for="row in rows" :key="row.node_id" class="mb-4 rounded-lg border border-border p-4">
          <div class="mb-3 flex items-center justify-between"><strong>{{ nodeName(row.node_id) }}</strong><Tag>{{ mode === 'remote' ? '远程下载' : '本地校验' }}</Tag></div>
          <Alert v-if="row.catalogError" class="mb-3" show-icon type="error" :message="row.catalogError"><template #action><Button size="small" :loading="row.loading" @click="loadRow(row)">重试</Button></template></Alert>
          <div class="grid grid-cols-1 gap-x-4 sm:grid-cols-3">
            <FormItem label="Incus 项目" required><Input v-model:value="row.project_name" class="font-mono" /></FormItem>
            <FormItem label="架构" required>
              <Select v-if="mode === 'remote'" v-model:value="row.architecture" :options="architectureOptions(row)" :loading="row.loading" placeholder="选择架构" @change="synchronizeArtifact(row)" />
              <Input v-else v-model:value="row.architecture" class="font-mono" placeholder="如 x86_64 / aarch64" />
            </FormItem>
            <FormItem label="cloud-init" required>
              <Select v-if="mode === 'remote'" :value="row.cloud_init ? 'cloud' : 'plain'" :options="cloudOptions(row)" @change="(value) => selectCloud(row, value)" />
              <Switch v-else v-model:checked="row.cloud_init" checked-children="已包含" un-checked-children="未包含" />
            </FormItem>
          </div>
          <FormItem v-if="mode === 'remote'" label="匹配的官方镜像" required :extra="row.fingerprint || '当前系统、版本、类型、架构与 cloud-init 组合无匹配镜像，请更改配置；不会替换成其他架构或变体。'">
            <Select v-model:value="row.fingerprint" :options="artifactOptions(row)" :loading="row.loading" :disabled="row.loading || artifacts(row).length === 0" show-search option-filter-prop="label" placeholder="选择镜像构件" @change="synchronizeArtifact(row)" />
          </FormItem>
          <FormItem v-else label="完整指纹（SHA-256）" required extra="64 位十六进制。incus image list -c lFd 或 incus image info <短指纹> 可查看完整值；节点中缺少该镜像会校验失败，不会从网络下载。"><Input v-model:value="row.fingerprint" class="font-mono" /></FormItem>
          <FormItem label="GPU 就绪（管理员确认）" extra="仅在该节点的镜像已包含所需驱动和用户态库时启用；下载官方镜像不会自动安装 GPU 环境。"><Switch v-model:checked="row.gpu_ready" /></FormItem>
        </section>
      </Form>
    </Spin>
    <template #footer><Space><Button @click="emit('update:open', false)">取消</Button><Button type="primary" :loading="action.pending.value" :disabled="setupLoading || rows.length === 0" @click="submit">{{ image ? '提交节点准备任务' : '登记并准备镜像' }}</Button></Space></template>
  </Drawer>
</template>
