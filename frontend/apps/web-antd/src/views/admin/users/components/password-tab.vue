<script lang="ts" setup>
import type { User } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Alert, Button, Form, FormItem, Input, Space } from 'ant-design-vue';

import { usersApi } from '#/api/backend';
import ConfirmAction from '#/components/confirm-action.vue';
import { useAction } from '#/composables/api';
import { useAuthStore } from '#/store';

import { PASSWORD_MAX, passwordError, randomPassword } from '#/utils/password';
import PasswordReveal from './password-reveal.vue';

const props = defineProps<{ etag: null | string; user: User }>();
const emit = defineEmits<{ changed: [] }>();
const auth = useAuthStore();

const password = ref('');
const error = computed(() => (password.value ? passwordError(password.value) : null));
const isSelf = computed(() => auth.me?.user.id === props.user.id);
const deleted = computed(() => props.user.status === 'deleted');

const action = useAction();
watch(password, () => action.resetKey());
const confirmOpen = ref(false);
const revealOpen = ref(false);
const revealed = ref('');

async function reset() {
  const newPassword = password.value;
  const r = await action.run(
    (key) => usersApi.resetPassword(props.user.id, { new_password: newPassword }, { ifMatch: props.etag, key }),
    { onConflict: () => emit('changed'), success: '密码已重置，该用户的所有会话已撤销' },
  );
  if (!r) return false;
  revealed.value = newPassword;
  password.value = '';
  revealOpen.value = true;
  if (!isSelf.value) emit('changed');
  return true;
}

/** 重置本人密码后后端已清除当前会话 Cookie，关闭弹窗后退出登录。 */
async function afterReveal() {
  revealed.value = '';
  if (isSelf.value) await auth.logout();
}
</script>

<template>
  <div class="max-w-xl">
    <Alert v-if="deleted" message="已删除的账号不能重置密码。" show-icon type="info" />
    <template v-else>
      <Alert
        class="mb-4"
        :message="
          isSelf
            ? '这是你自己的账号。重置后你的所有会话（包括当前浏览器）都会退出，需要用新密码重新登录。'
            : `重置后 ${user.username} 的所有登录会话会被撤销。`
        "
        show-icon
        :type="isSelf ? 'warning' : 'info'"
      />
      <Form layout="vertical">
        <FormItem
          :help="error ?? '12–128 个字符'"
          label="新密码"
          required
          :validate-status="error ? 'error' : undefined"
        >
          <Space.Compact class="w-full">
            <Input v-model:value="password" class="font-mono" :maxlength="PASSWORD_MAX" />
            <Button @click="password = randomPassword()">生成随机密码</Button>
          </Space.Compact>
        </FormItem>
        <Button
          danger
          :disabled="!password || !!error"
          :loading="action.pending.value"
          type="primary"
          @click="confirmOpen = true"
        >
          重置密码
        </Button>
      </Form>
    </template>

    <ConfirmAction
      v-model:open="confirmOpen"
      danger
      :description="`将重置 ${user.username} 的密码并撤销其所有会话。`"
      :on-confirm="reset"
      title="重置密码"
    />
    <PasswordReveal
      v-model:open="revealOpen"
      description="新密码只显示这一次，请复制后交给用户。"
      :password="revealed"
      title="新密码"
      @closed="afterReveal"
    />
  </div>
</template>
