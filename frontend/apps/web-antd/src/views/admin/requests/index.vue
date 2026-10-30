<script lang="ts" setup>
import type { CustomRequest } from '#/api/types';

import { onMounted, ref } from 'vue';
import { useRoute } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Card, Input, Select, Space, Table } from 'ant-design-vue';

import { requestsApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { useCursorList } from '#/composables/api';
import { ENUMS } from '#/locales/enums';
import { formatBytes } from '#/utils/format';

const route = useRoute();
const filters = ref<{ status?: CustomRequest['status']; user_id?: string }>({
  status: (route.query.status as CustomRequest['status']) || 'pending',
  user_id: (route.query.user_id as string) || undefined,
});
const list = useCursorList<CustomRequest, typeof filters.value>(requestsApi.list, filters);

const statusOptions = Object.entries(ENUMS.requestStatus ?? {}).map(([value, l]) => ({ label: l.text, value }));

const columns = [
  { dataIndex: 'id', title: 'ID', width: 70 },
  { key: 'name', title: '名称' },
  { key: 'user', title: '用户' },
  { key: 'status', title: '状态', width: 90 },
  { key: 'type', title: '类型', width: 80 },
  { key: 'resources', title: '申请资源' },
  { key: 'created', title: '提交', width: 120 },
  { key: 'reviewed', title: '审批', width: 120 },
  { key: 'instance', title: '实例', width: 100 },
];

function gpuText(r: CustomRequest): string {
  const g = r.requested_config.gpu;
  return g.count > 0 ? ` / ${g.count}× ${g.model ?? '未知型号'}` : '';
}

onMounted(() => list.load());
</script>

<template>
  <Page description="成员提交的自定义实例申请。批准时可修改配置，批准后交付任务进入任务中心。" title="自定义申请审批">
    <Card size="small">
      <Space class="mb-3" wrap>
        <Select v-model:value="filters.status" allow-clear class="w-32" :options="statusOptions" placeholder="全部状态" />
        <Input v-model:value="filters.user_id" allow-clear class="w-32" placeholder="用户 ID" />
      </Space>
      <Table
        :columns="columns"
        :data-source="list.items.value"
        :loading="list.loading.value"
        :pagination="false"
        row-key="id"
        size="small"
      >
        <template #bodyCell="{ column, record }">
          <template v-if="column.dataIndex === 'id'"><span class="font-mono">{{ record.id }}</span></template>
          <template v-else-if="column.key === 'name'">
            <RouterLink :to="`/admin/requests/${record.id}`">{{ record.name }}</RouterLink>
          </template>
          <template v-else-if="column.key === 'user'"><UserName :id="record.user_id" link /></template>
          <template v-else-if="column.key === 'status'"><StatusTag kind="requestStatus" :value="record.status" /></template>
          <template v-else-if="column.key === 'type'">
            <StatusTag kind="instanceType" :value="record.requested_config.instance_type" />
          </template>
          <template v-else-if="column.key === 'resources'">
            {{ record.requested_config.resources.vcpus }} 核 / {{ formatBytes(record.requested_config.resources.memory_bytes) }}
            / {{ formatBytes(record.requested_config.resources.disk_bytes) }}{{ gpuText(record as CustomRequest) }}
          </template>
          <template v-else-if="column.key === 'created'"><TimeText :value="record.created_at" /></template>
          <template v-else-if="column.key === 'reviewed'"><TimeText :value="record.reviewed_at" /></template>
          <template v-else-if="column.key === 'instance'">
            <RouterLink v-if="record.instance_id" :to="`/admin/instances/${record.instance_id}`">
              #{{ record.instance_id }}
            </RouterLink>
            <span v-else>—</span>
          </template>
        </template>
      </Table>
      <CursorPager
        :has-next="list.hasNext()"
        :has-prev="list.hasPrev()"
        :loading="list.loading.value"
        :page="list.page()"
        @next="list.nextPage"
        @prev="list.prevPage"
        @refresh="list.load"
      />
    </Card>
  </Page>
</template>
