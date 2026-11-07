<script lang="ts" setup>
import type {
  CreateCustomInstance,
  CreateLogin,
  CustomConfig,
  Instance,
  Node,
  PolicyVersions,
  User,
} from '#/api/types';

import {
  computed,
  onBeforeUnmount,
  onMounted,
  ref,
  shallowRef,
  watch,
} from 'vue';
import { useRoute } from 'vue-router';

import { Page } from '@vben/common-ui';
import { useAccessStore } from '@vben/stores';

import {
  Alert,
  Button,
  Card,
  Drawer,
  Form,
  FormItem,
  Input,
  message,
  Select,
  Space,
  Table,
  Tag,
} from 'ant-design-vue';

import { instancesApi, listAll, nodesApi, usersApi } from '#/api/backend';
import { ApiError, newIdempotencyKey } from '#/api/request';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import {
  errorToast,
  trackAccepted,
  useCursorList,
  usePolling,
} from '#/composables/api';
import {
  ENUMS,
  enumLabel,
  instanceState,
  instanceTaskState,
} from '#/locales/enums';
import { useAuthStore } from '#/store';
import { useUserCache } from '#/store/user-cache';
import { formatBytes } from '#/utils/format';
import { creationResultUncertain } from '#/utils/order-intent';

import BatchInstanceActions from './components/batch-instance-actions.vue';
import CustomInstanceForm from './components/custom-instance-form.vue';
import GraceTag from './components/grace-tag.vue';

const route = useRoute();
const q = (k: string) => (route.query[k] as string) || undefined;

type Filters = Record<
  'node_id' | 'observed_state' | 'owner_user_id' | 'product_type',
  string | undefined
>;
const filters = ref<Filters>({
  node_id: q('node_id'),
  observed_state: q('observed_state'),
  owner_user_id: q('owner_user_id'),
  product_type: q('product_type'),
});
const list = useCursorList<Instance>(instancesApi.list, filters);
const selected = ref<string[]>([]);
const selectedInstances = computed(() =>
  list.items.value.filter((instance) => selected.value.includes(instance.id)),
);
const rowSelection = computed(() => ({
  getCheckboxProps: (instance: Instance) => ({
    'aria-label': `选择实例 ${instance.name}`,
    disabled: list.loading.value || instance.desired_state === 'deleted',
  }),
  onChange: (keys: (number | string)[]) => {
    selected.value = keys.map(String);
  },
  selectedRowKeys: selected.value,
}));
watch(
  [() => list.page(), filters],
  () => {
    selected.value = [];
  },
  { deep: true, flush: 'sync' },
);
watch(list.items, (items) => {
  const visible = new Set(
    items
      .filter((instance) => instance.desired_state !== 'deleted')
      .map((instance) => instance.id),
  );
  selected.value = selected.value.filter((id) => visible.has(id));
});

const users = shallowRef<User[]>([]);
const nodes = shallowRef<Node[]>([]);
const userCache = useUserCache();

async function loadOptions() {
  try {
    const [u, n] = await Promise.all([
      listAll(usersApi.list),
      listAll(nodesApi.list),
    ]);
    users.value = u;
    nodes.value = n;
    for (const x of u) userCache.put(x.id, x.username);
  } catch (error) {
    errorToast(error);
  }
}

const userOptions = computed(() =>
  users.value
    .filter((u) => u.status !== 'deleted')
    .map((u) => ({ label: u.username, value: u.id })),
);
const nodeOptions = computed(() =>
  nodes.value.map((n) => ({ label: n.name, value: n.id })),
);
const nodeName = computed(() =>
  Object.fromEntries(nodes.value.map((n) => [n.id, n.name])),
);
const productOptions = Object.entries(ENUMS.productType ?? {}).map(
  ([value, l]) => ({ label: l.text, value }),
);
const stateOptions = Object.entries(ENUMS.observed ?? {}).map(([value, l]) => ({
  label: l.text,
  value,
}));

const busy = computed(() =>
  list.items.value.some(
    (i) =>
      i.active_task_id ||
      i.provisioning_state !== 'ready' ||
      i.observed_state === 'pending',
  ),
);
usePolling(
  () => list.load(),
  () => (busy.value ? 10_000 : 30_000),
);

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
const policyVersions = ref<PolicyVersions>({});
const login = shallowRef<CreateLogin | null>(null);
const createPending = ref(false);
const createUncertain = ref(false);
const createLocked = computed(
  () => createPending.value || createUncertain.value,
);
const auth = useAuthStore();
const access = useAccessStore();
const identity = computed(() =>
  auth.me?.user.role === 'admin' && !access.loginExpired
    ? auth.me.user.id
    : null,
);
let createSession = 0;
let intent: { body: CreateCustomInstance; key: string } | null = null;

function clearCreate() {
  ++createSession;
  intent = null;
  login.value = null;
  createPending.value = false;
  createUncertain.value = false;
  draft.value = { name: '', owner_user_id: undefined };
  config.value = null;
  policyVersions.value = {};
  formKey.value++;
}
watch(
  createOpen,
  (open) => {
    if (!open) clearCreate();
  },
  { flush: 'sync' },
);
watch(
  identity,
  () => {
    createOpen.value = false;
    clearCreate();
  },
  { flush: 'sync' },
);
onBeforeUnmount(clearCreate);

function openCreate() {
  if (!identity.value || createOpen.value) return;
  clearCreate();
  createOpen.value = true;
}

const canSubmit = computed(() => {
  const d = draft.value;
  const name = d.name.trim();
  return (
    !!identity.value &&
    !!d.owner_user_id &&
    name.length > 0 &&
    [...name].length <= 128 &&
    !!config.value &&
    !!login.value
  );
});

async function submitCreate() {
  if (createPending.value || !createOpen.value || !identity.value) return;
  if (!intent) {
    const ownerUserId = draft.value.owner_user_id;
    if (!canSubmit.value || !config.value || !login.value || !ownerUserId)
      return;
    // Detached original payload lives only in this component. Retrying never reads edited fields.
    intent = {
      body: JSON.parse(
        JSON.stringify({
          approved_config: config.value,
          login: login.value,
          name: draft.value.name.trim(),
          owner_user_id: ownerUserId,
          policy_versions: { ...policyVersions.value },
        }),
      ) as CreateCustomInstance,
      key: newIdempotencyKey(),
    };
  }
  const current = intent;
  const session = createSession;
  const retrying = createUncertain.value;
  createPending.value = true;
  try {
    const result = await instancesApi.createCustom(current.body, {
      key: current.key,
    });
    if (session !== createSession || !identity.value) return;
    trackAccepted(result.data, '下发自定义实例', () => list.load());
    message.success('已提交下发');
    createOpen.value = false;
    await list.load();
  } catch (error) {
    if (session !== createSession || !identity.value) return;
    createUncertain.value = creationResultUncertain(
      error instanceof ApiError ? error.status : undefined,
      retrying,
    );
    if (!createUncertain.value) intent = null;
    // Never display server-provided text/details from a credential-bearing request.
    errorToast(
      error instanceof ApiError
        ? new ApiError(
            error.status,
            { code: error.code, details: null, message: '' },
            error.requestId,
            error.retryAfter,
          )
        : new ApiError(0, undefined, '', null),
    );
  } finally {
    if (session === createSession) createPending.value = false;
  }
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
      <div class="mb-3 flex flex-wrap items-center gap-2">
        <Select
          v-model:value="filters.owner_user_id"
          allow-clear
          class="!w-40"
          :options="userOptions"
          option-filter-prop="label"
          placeholder="所有者"
          show-search
        />
        <Select
          v-model:value="filters.node_id"
          allow-clear
          class="!w-40"
          :options="nodeOptions"
          option-filter-prop="label"
          placeholder="节点"
          show-search
        />
        <Select
          v-model:value="filters.product_type"
          allow-clear
          class="!w-32"
          :options="productOptions"
          placeholder="产品类型"
        />
        <Select
          v-model:value="filters.observed_state"
          allow-clear
          class="!w-32"
          :options="stateOptions"
          placeholder="运行状态"
        />
      </div>
      <div class="mb-3 flex flex-wrap items-center gap-2">
        <span class="text-sm" aria-live="polite"
          >已选 {{ selected.length }} 个实例（仅当前页）</span
        >
        <BatchInstanceActions
          :instances="selectedInstances"
          :loading="list.loading.value"
          @accepted="
            (id) => {
              selected = selected.filter((value) => value !== id);
            }
          "
          @changed="list.load"
        />
        <Button :disabled="!selected.length" type="link" @click="selected = []"
          >清空选择</Button
        >
      </div>
      <Table
        :columns="columns"
        :data-source="list.items.value"
        :loading="list.loading.value"
        :pagination="false"
        :scroll="{ x: 'max-content' }"
        :row-selection="rowSelection"
        row-key="id"
        size="small"
      >
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'">
            <RouterLink :to="`/admin/instances/${record.id}`">{{
              record.name
            }}</RouterLink>
          </template>
          <template v-else-if="column.key === 'state'">
            <Space :size="4" wrap>
              <StatusTag :label="instanceState(row(record))" />
              <StatusTag
                v-if="record.active_task_id"
                :label="instanceTaskState(row(record))"
              />
              <GraceTag :enforcement="row(record).credit_enforcement" />
            </Space>
          </template>
          <template v-else-if="column.key === 'type'">
            <Space :size="4" wrap>
              <span>{{
                enumLabel('instanceType', record.instance_type).text
              }}</span>
              <Tag
                v-if="record.product_type === 'custom'"
                class="m-0"
                color="processing"
                >自定义</Tag
              >
              <Tag
                v-if="record.billing_mode === 'admin_exempt'"
                class="m-0"
                color="cyan"
                >免额度</Tag
              >
            </Space>
          </template>
          <template v-else-if="column.key === 'resources'">
            <span class="font-mono text-xs">
              {{ record.resources.vcpus }} vCPU /
              {{ formatBytes(record.resources.memory_bytes) }} /
              {{ formatBytes(record.resources.disk_bytes) }}
            </span>
          </template>
          <template v-else-if="column.key === 'owner'"
            ><UserName :id="record.user_id" link
          /></template>
          <template v-else-if="column.key === 'node'">
            <RouterLink :to="`/admin/nodes/${record.node_id}`">{{
              nodeName[record.node_id] ?? `节点 #${record.node_id}`
            }}</RouterLink>
          </template>
          <template v-else-if="column.key === 'created'"
            ><TimeText :value="record.created_at"
          /></template>
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

    <Drawer
      v-model:open="createOpen"
      destroy-on-close
      title="直接下发自定义实例"
      width="min(760px, 100vw)"
      :closable="!createPending"
      :mask-closable="!createPending"
      :keyboard="!createPending"
    >
      <Alert
        v-if="createUncertain"
        class="mb-4"
        show-icon
        type="warning"
        message="下发结果待确认"
        description="表单已锁定。重试只会发送原始配置和同一幂等键，不会新增创建意图。关闭将清除本地凭据；重新下发前请先在实例和任务列表核对，避免重复创建。"
      />
      <Form :disabled="createLocked" layout="vertical">
        <div class="grid grid-cols-1 gap-x-3 sm:grid-cols-2">
          <FormItem label="所有者" html-for="custom-owner" required>
            <Select
              id="custom-owner"
              v-model:value="draft.owner_user_id"
              :options="userOptions"
              option-filter-prop="label"
              placeholder="选择用户"
              show-search
            />
          </FormItem>
          <FormItem label="实例名称" html-for="custom-name" required>
            <Input
              id="custom-name"
              v-model:value="draft.name"
              :maxlength="128"
              placeholder="1–128 个字符"
            />
          </FormItem>
        </div>
      </Form>
      <CustomInstanceForm
        v-if="createOpen"
        :key="formKey"
        :disabled="createLocked"
        @update:value="(v) => (config = v)"
        @update:login="(v) => (login = v)"
        @update:policy-versions="(v) => (policyVersions = v)"
      />
      <div class="text-muted-foreground text-xs">
        自定义实例由后端定为免额度（admin_exempt）。会占用所有者的实例数与常驻实例配额，配额不足时返回"超出资源配额"。
      </div>
      <template #footer>
        <div class="flex justify-end gap-2">
          <Button :disabled="createPending" @click="createOpen = false"
            >关闭</Button
          >
          <Button
            :disabled="createPending || (!createUncertain && !canSubmit)"
            :loading="createPending"
            type="primary"
            @click="submitCreate"
            >{{ createUncertain ? '重试原始下发' : '下发' }}</Button
          >
        </div>
      </template>
    </Drawer>
  </Page>
</template>
