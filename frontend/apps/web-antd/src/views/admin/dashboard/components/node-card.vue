<script lang="ts" setup>
import type { Node, NodeResources } from '#/api/types';

import { computed } from 'vue';

import { Card, Descriptions, DescriptionsItem, Space, Tag } from 'ant-design-vue';

import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { formatAge, formatBytes, secondsSince } from '#/utils/format';

/** resources 为 null 表示 GET /admin/nodes/{id}/resources 请求失败。 */
const props = defineProps<{ node: Node; resources: NodeResources | null }>();

/** 库存超过 60 秒视为过期（与后端准入一致）。 */
const age = computed(() => secondsSince(props.node.resources_sampled_at));
const stale = computed(() => age.value === null || age.value > 60);

const host = computed(() => props.resources?.resources?.server_resources ?? null);
const bytes = (n: number | undefined) => (n === undefined ? null : formatBytes(String(n)));
const memUsed = computed(() => bytes(host.value?.memory?.used));
const memTotal = computed(() => bytes(host.value?.memory?.total));
const cpuTotal = computed(() => host.value?.cpu?.total ?? null);

const a = computed(() => props.node.allocatable);
</script>

<template>
  <Card size="small">
    <template #title>
      <Space>
        <RouterLink :to="`/admin/nodes/${node.id}`">{{ node.name }}</RouterLink>
        <StatusTag kind="adminState" :value="node.administrative_state" />
        <StatusTag kind="health" :value="node.health" />
      </Space>
    </template>
    <Descriptions :column="1" size="small">
      <DescriptionsItem label="连接验证"><TimeText :value="node.connection_verified_at" /></DescriptionsItem>
      <DescriptionsItem label="库存时间">
        <Space :size="4">
          <TimeText :value="node.resources_sampled_at" />
          <span :class="stale ? 'text-orange-500' : 'text-muted-foreground'">{{ formatAge(age) }}</span>
          <Tag v-if="stale" :bordered="false" class="m-0" color="warning">库存已过期</Tag>
        </Space>
      </DescriptionsItem>
      <DescriptionsItem label="可分配">
        <span class="font-mono text-xs">
          {{ a.vcpus }} vCPU / {{ formatBytes(a.memory_bytes) }} / {{ formatBytes(a.disk_bytes) }}
        </span>
      </DescriptionsItem>
      <DescriptionsItem label="常驻预算">
        <span class="font-mono text-xs">
          {{ a.resident_vcpus }} vCPU / {{ formatBytes(a.resident_memory_bytes) }} /
          {{ formatBytes(a.resident_disk_bytes) }}
        </span>
      </DescriptionsItem>
      <DescriptionsItem label="宿主内存">
        <span class="font-mono text-xs">{{ memUsed ?? '未知' }} / {{ memTotal ?? '未知' }}</span>
        <span v-if="resources" class="ml-2"><StatusTag kind="quality" :value="resources.quality" /></span>
      </DescriptionsItem>
      <DescriptionsItem label="宿主 CPU">
        <span class="font-mono text-xs">{{ cpuTotal === null ? '未知' : `${cpuTotal} 核` }}</span>
      </DescriptionsItem>
    </Descriptions>
  </Card>
</template>
