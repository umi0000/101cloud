<script lang="ts" setup>
import type { CatalogRefs } from '../../catalog/refs';

import type { CustomConfig } from '#/api/types';

import { Descriptions, DescriptionsItem, Tag } from 'ant-design-vue';

import StatusTag from '#/components/status-tag.vue';
import { CUSTOM_ACTION_NAMES, GPU_PERMISSION_NAMES } from '#/locales/enums';
import { formatBytes, formatPt } from '#/utils/format';

import { DIFF_CLASS } from './config';

const props = defineProps<{ config: CustomConfig; diff?: Set<string>; refs: CatalogRefs }>();

const cls = (key: string) => (props.diff?.has(key) ? DIFF_CLASS : '');
</script>

<template>
  <Descriptions bordered :column="1" size="small">
    <DescriptionsItem label="类型">
      <span :class="cls('instance_type')"><StatusTag kind="instanceType" :value="config.instance_type" /></span>
    </DescriptionsItem>
    <DescriptionsItem label="可用区"><span :class="cls('zone_id')">{{ refs.zoneName(config.zone_id) }}</span></DescriptionsItem>
    <DescriptionsItem label="镜像"><span :class="cls('image_id')">{{ refs.imageName(config.image_id) }}</span></DescriptionsItem>
    <DescriptionsItem label="网络">
      <span :class="cls('network_option_id')">{{ refs.bindingName(config.network_option_id) }}</span>
    </DescriptionsItem>
    <DescriptionsItem label="vCPU"><span :class="cls('vcpus')">{{ config.resources.vcpus }} 核</span></DescriptionsItem>
    <DescriptionsItem label="内存">
      <span :class="cls('memory_bytes')">{{ formatBytes(config.resources.memory_bytes) }}</span>
    </DescriptionsItem>
    <DescriptionsItem label="磁盘">
      <span :class="cls('disk_bytes')">{{ formatBytes(config.resources.disk_bytes) }}</span>
    </DescriptionsItem>
    <DescriptionsItem label="GPU">
      <span :class="cls('gpu_count')">{{ config.gpu.count }} 张</span>
      <span v-if="config.gpu.model" class="ml-2" :class="cls('gpu_model')">{{ config.gpu.model }}</span>
      <span class="ml-2" :class="cls('access_mode')">{{ config.gpu.access_mode === 'shared' ? '共享' : '独占' }}</span>
      <span class="ml-2 font-mono" :class="cls('max_hourly_pt')">≤ {{ formatPt(config.gpu.max_hourly_pt) }} pt/h</span>
    </DescriptionsItem>
    <DescriptionsItem v-if="config.gpu.fixed_uuids?.length" label="指定 GPU">
      <div v-for="u in config.gpu.fixed_uuids" :key="u" class="font-mono text-xs">{{ u }}</div>
    </DescriptionsItem>
    <DescriptionsItem v-if="config.node_id" label="指定节点">{{ refs.nodeName(config.node_id) }}</DescriptionsItem>
    <DescriptionsItem label="初始化模板">
      <span :class="cls('init_template_version_id')">{{ refs.templateName(config.init_template_version_id) }}</span>
    </DescriptionsItem>
    <DescriptionsItem label="初始化参数">
      <pre class="m-0 whitespace-pre-wrap font-mono text-xs" :class="cls('init_parameters')">{{ JSON.stringify(config.init_parameters ?? {}, null, 2) }}</pre>
    </DescriptionsItem>
    <DescriptionsItem v-if="config.allowed_user_actions" label="成员可执行操作">
      <Tag v-for="a in config.allowed_user_actions" :key="a">{{ CUSTOM_ACTION_NAMES[a] ?? a }}</Tag>
      <span v-if="config.allowed_user_actions.length === 0" class="text-muted-foreground">无</span>
    </DescriptionsItem>
    <DescriptionsItem v-if="config.gpu_user_permissions" label="成员可改 GPU 策略">
      <template v-for="(v, k) in config.gpu_user_permissions" :key="k">
        <Tag v-if="v">{{ GPU_PERMISSION_NAMES[k] ?? k }}</Tag>
      </template>
      <span v-if="!Object.values(config.gpu_user_permissions).some(Boolean)" class="text-muted-foreground">无</span>
    </DescriptionsItem>
  </Descriptions>
</template>
