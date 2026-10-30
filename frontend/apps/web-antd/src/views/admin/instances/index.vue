<script lang="ts" setup>
import type { CustomConfig, Instance, Node, User } from '#/api/types';

import { computed, onMounted, ref, shallowRef, watch } from 'vue';
import { useRoute } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Button, Card, Drawer, Form, FormItem, Input, Select, Space, Table, Tag } from 'ant-design-vue';

import { instancesApi, listAll, nodesApi, usersApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { errorToast, trackAccepted, useAction, useCursorList, usePolling } from '#/composables/api';
import { ENUMS, enumLabel, instanceState } from '#/locales/enums';
import { useUserCache } from '#/store/user-cache';
import { formatBytes } from '#/utils/format';

import CustomInstanceForm from './components/custom-instance-form.vue';
import GraceTag from './components/grace-tag.vue';

const route = useRoute();
const q = (k: string) => (route.query[k] as string) || undefined;

type Filters = Record<'node_id' | 'observed_state' | 'owner_user_id' | 'product_type', string | undefined>;
const filters = ref<Filters>({
  node_id: q('node_id'),
  observed_state: q('observed_state'),
  owner_user_id: q('owner_user_id'),
  product_type: q('product_type'),
});
const list = useCursorList<Instance>(instancesApi.list, filters);

const users = shallowRef<User[]>([]);
const nodes = shallowRef<Node[]>([]);
const userCache = useUserCache();

async function loadOptions() {
  try {
    const [u, n] = await Promise.all([listAll(usersApi.list), listAll(nodesApi.list)]);
    users.value = u;
    nodes.value = n;
    for (const x of u) userCache.put(x.id, x.username);
  } catch (error) {
    errorToast(error);
  }
}

const userOptions = computed(() =>
  users.value.filter((u) => u.status !== 'deleted').map((u) => ({ label: u.username, value: u.id })),
);
const nodeOptions = computed(() => nodes.value.map((n) => ({ label: n.name, value: n.id })));
const nodeName = computed(() => Object.fromEntries(nodes.value.map((n) => [n.id, n.name])));
const productOptions = Object.entries(ENUMS.productType ?? {}).map(([value, l]) => ({ label: l.text, value }));
const stateOptions = Object.entries(ENUMS.observed ?? {}).map(([value, l]) => ({ label: l.text, value }));

const busy = computed(() =>
  list.items.value.some(
    (i) => i.active_task_id || i.provisioning_state !== 'ready' || i.observed_state === 'pending',
  ),
);
usePolling(() => list.load(), () => (busy.value ? 10_000 : 30_000));

const columns = [
  { key: 'name', title: '名称' },
  { key: 'state', title: '状态' },
  { key: 'type', title: '类型' },
  { key: 'resources', title: '规格' },
  { key: 'owner', title: '所有者' },
  { key: 'node', title: '节点' },
  { key: 'created', title: '创建时间', width: 130 },
];

function row(record: Record<string, unknown>): Instance {
  return record as unknown as Instance;
}

/* ---------- 直接下发自定义实例 ---------- */

const createOpen = ref(false);
const formKey = ref(0);
const draft = ref({ name: '', owner_user_id: undefined as string | undefined });
const config = shallowRef<CustomConfig | null>(null);
const create = useAction();

watch([draft, config], () => create.resetKey(), { deep: true });

function openCreate() {
  draft.value = { name: '', owner_user_id: undefined };
  config.value = null;
  formKey.value++;
  createOpen.value = true;
}

const canSubmit = computed(() => {
  const d = draft.value;
  const name = d.name.trim();
  return !!d.owner_user_id && name.length > 0 && [...name].length <= 128 && !!config.value;
});

async function submitCreate() {
  const d = draft.value;
  const ownerUserId = d.owner_user_id;
  if (!canSubmit.value || !config.value || !ownerUserId) return;
  const body = { approved_config: config.value, name: d.name.trim(), owner_user_id: ownerUserId };
  const r = await create.run((key) => instancesApi.createCustom(body, { key }), { success: '已提交下发' });
  if (!r) return;
  trackAccepted(r.data, '下发自定义实例', () => list.load());
  createOpen.value = false;
  await list.load();
}

onMounted(() => {
  void loadOptions();
  void list.load();
});
</script>

<template>
  <Page title="实例">
    <template #extra>
      <Button type="primary" @click="openCreate">直接下发自定义实例</Button>
    </template>
    <Card size="small">
      <Space class="mb-3" wrap>
        <Select
          v-model:value="filters.owner_user_id"
          allow-clear
          class="w-40"
          :options="userOptions"
          option-filter-prop="label"
          placeholder="所有者"
          show-search
        />
        <Select
          v-model:value="filters.node_id"
          allow-clear
          class="w-40"
          :options="nodeOptions"
          option-filter-prop="label"
          placeholder="节点"
          show-search
        />
        <Select v-model:value="filters.product_type" allow-clear class="w-32" :options="productOptions" placeholder="产品类型" />
        <Select v-model:value="filters.observed_state" allow-clear class="w-32" :options="stateOptions" placeholder="运行状态" />
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
          <template v-if="column.key === 'name'">
            <RouterLink :to="`/admin/instances/${record.id}`">{{ record.name }}</RouterLink>
          </template>
          <template v-else-if="column.key === 'state'">
            <Space :size="4" wrap>
              <StatusTag :label="instanceState(row(record))" />
              <Tag v-if="record.active_task_id" class="m-0" color="processing">操作中</Tag>
              <GraceTag :enforcement="row(record).credit_enforcement" />
            </Space>
          </template>
          <template v-else-if="column.key === 'type'">
            <Space :size="4" wrap>
              <span>{{ enumLabel('instanceType', record.instance_type).text }}</span>
              <Tag v-if="record.product_type === 'custom'" class="m-0" color="processing">自定义</Tag>
              <Tag v-if="record.billing_mode === 'admin_exempt'" class="m-0" color="cyan">免额度</Tag>
            </Space>
          </template>
          <template v-else-if="column.key === 'resources'">
            <span class="font-mono text-xs">
              {{ record.resources.vcpus }} vCPU / {{ formatBytes(record.resources.memory_bytes) }} / {{ formatBytes(record.resources.disk_bytes) }}
            </span>
          </template>
          <template v-else-if="column.key === 'owner'"><UserName :id="record.user_id" link /></template>
          <template v-else-if="column.key === 'node'">
            <RouterLink :to="`/admin/nodes/${record.node_id}`">{{ nodeName[record.node_id] ?? `节点 #${record.node_id}` }}</RouterLink>
          </template>
          <template v-else-if="column.key === 'created'"><TimeText :value="record.created_at" /></template>
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

    <Drawer v-model:open="createOpen" destroy-on-close title="直接下发自定义实例" :width="760">
      <Form layout="vertical">
        <div class="grid grid-cols-2 gap-x-3">
          <FormItem label="所有者" required>
            <Select
              v-model:value="draft.owner_user_id"
              :options="userOptions"
              option-filter-prop="label"
              placeholder="选择用户"
              show-search
            />
          </FormItem>
          <FormItem label="实例名称" required>
            <Input v-model:value="draft.name" :maxlength="128" placeholder="1–128 个字符" />
          </FormItem>
        </div>
      </Form>
      <CustomInstanceForm :key="formKey" @update:value="(v) => (config = v)" />
      <div class="text-muted-foreground text-xs">
        自定义实例由后端定为免额度（admin_exempt）。会占用所有者的实例数与常驻实例配额，配额不足时返回"超出资源配额"。
      </div>
      <template #footer>
        <div class="flex justify-end gap-2">
          <Button @click="createOpen = false">取消</Button>
          <Button :disabled="!canSubmit" :loading="create.pending.value" type="primary" @click="submitCreate">下发</Button>
        </div>
      </template>
    </Drawer>
  </Page>
</template>
