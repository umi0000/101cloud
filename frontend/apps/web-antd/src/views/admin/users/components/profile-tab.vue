<script lang="ts" setup>
import type { Role, User } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Alert, Button, Descriptions, DescriptionsItem, Form, FormItem, Input, Select } from 'ant-design-vue';

import { usersApi } from '#/api/backend';
import { ApiError } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { useAction } from '#/composables/api';
import { enumLabel } from '#/locales/enums';
import { useAuthStore } from '#/store';

type Patch = Partial<{ email: null | string; role: Role; status: 'active' | 'disabled' }>;

const props = defineProps<{ etag: null | string; user: User }>();
const emit = defineEmits<{ changed: [] }>();
const auth = useAuthStore();

const form = ref({ email: '', role: 'user' as Role, status: 'active' as 'active' | 'disabled' });
function fill(u: User) {
  form.value = { email: u.email ?? '', role: u.role, status: u.status === 'disabled' ? 'disabled' : 'active' };
}
watch(() => props.user, fill, { immediate: true });

const deleted = computed(() => props.user.status === 'deleted');
const isSelf = computed(() => auth.me?.user.id === props.user.id);

const roleOptions = (['user', 'admin'] as const).map((value) => ({ label: enumLabel('role', value).text, value }));
const statusOptions = (['active', 'disabled'] as const).map((value) => ({
  label: enumLabel('userStatus', value).text,
  value,
}));

const emailError = computed(() => ([...form.value.email.trim()].length > 254 ? '邮箱最多 254 个字符' : null));

/** 只提交变化的字段。 */
const patch = computed<Patch>(() => {
  const out: Patch = {};
  const u = props.user;
  const email = form.value.email.trim() || null;
  if (email !== u.email) out.email = email;
  if (form.value.role !== u.role) out.role = form.value.role;
  if (form.value.status !== u.status) out.status = form.value.status;
  return out;
});
const dirty = computed(() => Object.keys(patch.value).length > 0);
/** 禁用或改角色会撤销该用户所有会话。 */
const revokes = computed(() => patch.value.status === 'disabled' || patch.value.role !== undefined);

const save = useAction();
watch(form, () => save.resetKey(), { deep: true });
const lastAdmin = ref(false);
const confirmOpen = ref(false);

async function submit() {
  lastAdmin.value = false;
  const body = patch.value;
  const revokesSelf = isSelf.value && revokes.value;
  const r = await save.run(
    async () => {
      try {
        return await usersApi.update(props.user.id, body, { ifMatch: props.etag });
      } catch (error) {
        if (error instanceof ApiError && error.code === 'invalid_state') lastAdmin.value = true;
        throw error;
      }
    },
    { onConflict: () => emit('changed'), success: '已保存' },
  );
  if (!r) return false;
  if (revokesSelf) {
    await auth.logout();
    return true;
  }
  emit('changed');
  return true;
}

function onSave() {
  if (!dirty.value || emailError.value) return;
  if (revokes.value) confirmOpen.value = true;
  else void submit();
}
</script>

<template>
  <div class="grid gap-4 lg:grid-cols-2">
    <Descriptions bordered :column="1" size="small">
      <DescriptionsItem label="ID"><span class="font-mono">{{ user.id }}</span></DescriptionsItem>
      <DescriptionsItem label="用户名">{{ user.username }}</DescriptionsItem>
      <DescriptionsItem label="角色"><StatusTag kind="role" :value="user.role" /></DescriptionsItem>
      <DescriptionsItem label="状态"><StatusTag kind="userStatus" :value="user.status" /></DescriptionsItem>
      <DescriptionsItem label="邮箱">
        <span v-if="user.email" class="font-mono">{{ user.email }}</span>
        <span v-else class="text-muted-foreground">—</span>
      </DescriptionsItem>
      <DescriptionsItem label="创建时间"><TimeText :value="user.created_at" /></DescriptionsItem>
    </Descriptions>

    <div>
      <Alert v-if="deleted" message="已删除的账号不能修改。" show-icon type="info" />
      <Form v-else layout="vertical">
        <FormItem
          :help="emailError ?? '留空表示清除邮箱'"
          label="邮箱"
          :validate-status="emailError ? 'error' : undefined"
        >
          <Input v-model:value="form.email" :maxlength="254" />
        </FormItem>
        <FormItem label="角色">
          <Select v-model:value="form.role" :options="roleOptions" />
        </FormItem>
        <FormItem label="状态">
          <Select v-model:value="form.status" :options="statusOptions" />
        </FormItem>
        <Alert
          v-if="lastAdmin"
          class="mb-3"
          message="不能禁用或降级最后一个有效管理员，或该账号当前状态不允许修改。"
          show-icon
          type="error"
        />
        <Button
          :disabled="!dirty || !!emailError"
          :loading="save.pending.value"
          type="primary"
          @click="onSave"
        >
          保存
        </Button>
        <Button class="ml-2" :disabled="!dirty" @click="fill(user)">还原</Button>
      </Form>
    </div>

    <ConfirmAction
      v-model:open="confirmOpen"
      danger
      :description="`${patch.status === 'disabled' ? '禁用账号' : '修改角色'}会立即撤销 ${user.username} 的所有登录会话，但不会自动停止或删除其实例。${isSelf ? '这是你自己的账号，当前浏览器也会退出。' : ''}`"
      :on-confirm="submit"
      title="确认修改账号"
    />
  </div>
</template>
