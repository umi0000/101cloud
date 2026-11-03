<script lang="ts" setup>
import type { PolicyVersion, Role, User, UserStatus } from '#/api/types';

import { computed, onMounted, ref, watch } from 'vue';
import { useRouter } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Alert, Button, Card, Form, FormItem, Input, Modal, Select, Space, Table } from 'ant-design-vue';

import { listAll, policiesApi, usersApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { errorToast, useAction, useCursorList } from '#/composables/api';
import { enumLabel } from '#/locales/enums';
import { useUserCache } from '#/store/user-cache';

import { isEffective } from '../finance/components/credit-policy';

import PasswordReveal from './components/password-reveal.vue';
import { PASSWORD_MAX, passwordError, randomPassword } from './password';

const router = useRouter();
const cache = useUserCache();

type UserFilters = { role?: Role; status?: UserStatus; username?: string };
const filters = ref<UserFilters>({});
const usernameInput = ref('');
const list = useCursorList<User, UserFilters>(usersApi.list, filters);

const statusOptions = (['active', 'disabled', 'deleted'] as const).map((value) => ({
  label: enumLabel('userStatus', value).text,
  value,
}));
const roleOptions = (['user', 'admin'] as const).map((value) => ({ label: enumLabel('role', value).text, value }));

/** username 为精确匹配；后端拒绝首尾空白。 */
function applyUsername() {
  filters.value = { ...filters.value, username: usernameInput.value.trim() || undefined };
}

watch(list.items, (items) => {
  for (const u of items) cache.put(u.id, u.username);
});

const columns = [
  { key: 'username', title: '用户名' },
  { key: 'role', title: '角色', width: 100 },
  { key: 'status', title: '状态', width: 100 },
  { key: 'email', title: '邮箱' },
  { key: 'created', title: '创建时间', width: 150 },
];

/* ---------- 创建用户 ---------- */

const createOpen = ref(false);
const form = ref({ credit_policy_version_id: '', email: '', initial_password: '', role: 'user' as Role, username: '' });
const create = useAction();
watch(form, () => create.resetKey(), { deep: true });

const creditVersions = ref<PolicyVersion[]>([]);
const policiesLoading = ref(false);
const policiesFailed = ref(false);
const policyOptions = computed(() =>
  creditVersions.value.filter((v) => isEffective(v)).map((v) => ({ label: `${v.code} v${v.version}`, value: v.id })),
);

async function loadPolicies() {
  policiesLoading.value = true;
  policiesFailed.value = false;
  try {
    creditVersions.value = await listAll((q) => policiesApi.list('credit', q));
  } catch (error) {
    policiesFailed.value = true;
    errorToast(error);
  } finally {
    policiesLoading.value = false;
  }
}

const errors = computed(() => {
  const f = form.value;
  const n = [...f.username].length;
  return {
    email: [...f.email.trim()].length > 254 ? '邮箱最多 254 个字符' : null,
    password: passwordError(f.initial_password),
    username:
      n < 1 || n > 64 ? '用户名需 1–64 个字符' : f.username.trim() === f.username ? null : '用户名首尾不能有空格',
  };
});
const formValid = computed(() =>
  !errors.value.username && !errors.value.email && !errors.value.password
  && !policiesLoading.value && !policiesFailed.value
  && policyOptions.value.some((p) => p.value === form.value.credit_policy_version_id),
);

function openCreate() {
  form.value = { credit_policy_version_id: '', email: '', initial_password: '', role: 'user', username: '' };
  createOpen.value = true;
  void loadPolicies();
}

const created = ref<null | { id: string; password: string; username: string }>(null);
const revealOpen = ref(false);

async function submitCreate() {
  if (!formValid.value) return;
  const f = form.value;
  const email = f.email.trim();
  const r = await create.run((key) =>
    usersApi.create(
      {
        credit_policy_version_id: f.credit_policy_version_id,
        email: email || null,
        initial_password: f.initial_password,
        role: f.role,
        username: f.username,
      },
      { key },
    ),
  );
  if (!r) return;
  cache.put(r.data.id, r.data.username);
  created.value = { id: r.data.id, password: f.initial_password, username: r.data.username };
  createOpen.value = false;
  revealOpen.value = true;
}

function afterReveal() {
  const id = created.value?.id;
  created.value = null;
  form.value.initial_password = '';
  if (id) void router.push({ path: `/admin/users/${id}`, query: { tab: 'limits' } });
}

onMounted(() => list.load());
</script>

<template>
  <Page title="用户与配额">
    <Card size="small">
      <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
        <Space wrap>
          <Select v-model:value="filters.status" allow-clear class="w-32" :options="statusOptions" placeholder="状态" />
          <Select v-model:value="filters.role" allow-clear class="w-32" :options="roleOptions" placeholder="角色" />
          <Input.Search
            v-model:value="usernameInput"
            allow-clear
            class="w-56"
            placeholder="精确用户名"
            @search="applyUsername"
          />
        </Space>
        <Button type="primary" @click="openCreate">创建用户</Button>
      </div>
      <Table
        :columns="columns"
        :data-source="list.items.value"
        :loading="list.loading.value"
        :pagination="false"
        row-key="id"
        size="small"
      >
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'username'">
            <RouterLink :to="`/admin/users/${record.id}`">{{ record.username }}</RouterLink>
            <span class="text-muted-foreground ml-1 font-mono text-xs">#{{ record.id }}</span>
          </template>
          <template v-else-if="column.key === 'role'"><StatusTag kind="role" :value="record.role" /></template>
          <template v-else-if="column.key === 'status'"><StatusTag kind="userStatus" :value="record.status" /></template>
          <template v-else-if="column.key === 'email'">
            <span v-if="record.email" class="font-mono">{{ record.email }}</span>
            <span v-else class="text-muted-foreground">—</span>
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

    <Modal
      v-model:open="createOpen"
      :confirm-loading="create.pending.value"
      destroy-on-close
      :ok-button-props="{ disabled: !formValid }"
      ok-text="创建"
      title="创建用户"
      @ok="submitCreate"
    >
      <Form layout="vertical">
        <FormItem
          :help="form.username ? (errors.username ?? undefined) : undefined"
          label="用户名"
          required
          :validate-status="form.username && errors.username ? 'error' : undefined"
        >
          <Input v-model:value="form.username" :maxlength="64" />
        </FormItem>
        <FormItem
          :help="errors.email ?? undefined"
          label="邮箱（可选）"
          :validate-status="errors.email ? 'error' : undefined"
        >
          <Input v-model:value="form.email" :maxlength="254" />
        </FormItem>
        <FormItem
          :help="form.initial_password ? (errors.password ?? undefined) : '12–128 个字符'"
          label="初始密码"
          required
          :validate-status="form.initial_password && errors.password ? 'error' : undefined"
        >
          <Space.Compact class="w-full">
            <Input v-model:value="form.initial_password" class="font-mono" :maxlength="PASSWORD_MAX" />
            <Button @click="form.initial_password = randomPassword()">生成随机密码</Button>
          </Space.Compact>
        </FormItem>
        <FormItem label="角色" required>
          <Select v-model:value="form.role" :options="roleOptions" />
        </FormItem>
        <FormItem
          help="用户共享钱包的每日额度规则独立于实例套餐；选择套餐不会发放额度。"
          label="共享钱包额度政策"
          required
        >
          <Select
            v-model:value="form.credit_policy_version_id"
            class="w-full"
            :disabled="create.pending.value || policiesLoading || policiesFailed"
            :loading="policiesLoading"
            option-filter-prop="label"
            :options="policyOptions"
            placeholder="请选择已生效的额度政策"
            show-search
          />
          <Alert v-if="policiesFailed" class="mt-2" message="额度政策加载失败" show-icon type="error">
            <template #action><Button size="small" @click="loadPolicies">重试</Button></template>
          </Alert>
          <Alert
            v-else-if="!policiesLoading && !policyOptions.length"
            class="mt-2"
            message="暂无已生效的额度政策，请先在政策配置中发布，再刷新选项。"
            show-icon
            type="warning"
          >
            <template #action><Button size="small" @click="loadPolicies">刷新</Button></template>
          </Alert>
        </FormItem>
      </Form>
      <Alert
        message="新用户配额全部为 0，创建后请在“配额”中授予资源。共享钱包将绑定所选政策；创建账户本身不发放额度。"
        show-icon
        type="info"
      />
    </Modal>

    <PasswordReveal
      v-model:open="revealOpen"
      :description="`用户 ${created?.username ?? ''} 已创建。初始密码只显示这一次，关闭后无法再查看。`"
      :password="created?.password ?? ''"
      title="初始密码"
      @closed="afterReveal"
    />
  </Page>
</template>
