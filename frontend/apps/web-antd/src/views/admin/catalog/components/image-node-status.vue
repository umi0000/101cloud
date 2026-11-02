<script lang="ts" setup>
import type { Image, ImageNode, ImageNodeInput, Node, Task } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Button, Space, Tag, Tooltip } from 'ant-design-vue';

import { catalogApi, opsApi } from '#/api/backend';
import { revisionTag } from '#/api/request';
import { errorToast, trackAccepted, useAction, usePolling } from '#/composables/api';
import { shortId } from '#/utils/format';

const props = defineProps<{ image: Image; nodes: Node[] }>();
const emit = defineEmits<{ changed: [] }>();
const action = useAction();
const tasks = ref<Record<string, Task>>({});
const states = {
  pending: { color: 'default', text: '等待准备' },
  downloading: { color: 'processing', text: '下载 / 校验中' },
  ready: { color: 'success', text: '就绪' },
  failed: { color: 'error', text: '失败' },
  unknown: { color: 'warning', text: '结果未知' },
} as const;
const active = computed(() => props.image.nodes.some((n) => n.status === 'pending' || n.status === 'downloading'));
function nodeName(id: string) { return props.nodes.find((n) => n.id === id)?.name ?? `节点 #${id}`; }
function progress(node: ImageNode) {
  if (node.status === 'ready') return '';
  const value = node.task_id ? tasks.value[node.task_id]?.result?.progress : undefined;
  return typeof value === 'string' ? value : '';
}
async function loadTasks() {
  const ids = [...new Set(props.image.nodes.filter((n) => n.task_id && n.status !== 'ready').map((n) => n.task_id))];
  await Promise.all(ids.map(async (id) => {
    if (!id) return;
    const result = await opsApi.task(id);
    tasks.value[id] = result.data;
  }));
}
watch(() => props.image.nodes.map((n) => `${n.task_id}:${n.status}`).join(','), () => { void loadTasks().catch(errorToast); }, { immediate: true });
usePolling(async () => { if (active.value) await loadTasks(); }, () => 3000);
async function retry(node: ImageNode) {
  const input: ImageNodeInput = {
    node_id: node.node_id, project_name: node.project_name, architecture: node.architecture,
    fingerprint: node.fingerprint, cloud_init: node.cloud_init, gpu_ready: node.gpu_ready,
    properties: node.properties, variant: node.variant,
    ...(node.source_id ? { source_id: node.source_id } : {}),
  };
  const response = await action.run((key) => catalogApi.prepareImageNodes(props.image.id, [input], { key, ifMatch: revisionTag(props.image.revision) }), { success: '已提交重试任务', onConflict: () => emit('changed') });
  if (!response) return;
  for (const task of response.data.tasks) trackAccepted({ task_id: task.task_id, status: 'queued', instance_id: null }, `重试镜像 · ${nodeName(task.node_id)}`, () => emit('changed'));
  action.resetKey();
  emit('changed');
}
async function reconcile(node: ImageNode) {
  const taskId = node.task_id;
  if (!taskId) return;
  const detail = await action.run(() => opsApi.task(taskId));
  if (!detail) return;
  const response = await action.run((key) => opsApi.reconcile(detail.data.id, { key, ifMatch: detail.etag ?? revisionTag(detail.data.revision) }), { success: '已请求核对原下载任务', onConflict: () => emit('changed') });
  if (!response) return;
  trackAccepted(response.data, `核对镜像 · ${nodeName(node.node_id)}`, () => emit('changed'));
  action.resetKey();
  emit('changed');
}
</script>

<template>
  <div class="min-w-72 space-y-3">
    <div v-if="image.nodes.length === 0" class="text-muted-foreground">尚未配置节点</div>
    <div v-for="node in image.nodes" :key="node.id" class="space-y-1">
      <Space wrap size="small">
        <span class="font-medium">{{ nodeName(node.node_id) }}</span>
        <Tag :color="states[node.status].color">{{ states[node.status].text }}</Tag>
        <span class="font-mono text-xs">{{ node.project_name }} · {{ node.architecture }}</span>
        <Tag v-if="node.cloud_init">cloud-init</Tag><Tag v-if="node.gpu_ready" color="success">GPU 就绪</Tag>
      </Space>
      <div class="font-mono text-xs text-muted-foreground"><Tooltip :title="node.fingerprint">{{ shortId(node.fingerprint) }}</Tooltip><span v-if="progress(node)" class="ml-2">{{ progress(node) }}</span><span v-if="node.task_id" class="ml-2">任务 #{{ node.task_id }}</span></div>
      <div v-if="node.error" class="text-xs text-destructive">{{ node.error.message || node.error.code }}</div>
      <Space v-if="node.status === 'failed' || node.status === 'unknown'" size="small">
        <Button v-if="node.status === 'failed'" :loading="action.pending.value" size="small" @click="retry(node)">重试失败节点</Button>
        <Button v-if="node.status === 'unknown' && node.task_id" :loading="action.pending.value" size="small" @click="reconcile(node)">核对原任务</Button>
        <span v-if="node.status === 'unknown'" class="text-xs text-muted-foreground">先核对，不重复下载</span>
      </Space>
    </div>
  </div>
</template>
