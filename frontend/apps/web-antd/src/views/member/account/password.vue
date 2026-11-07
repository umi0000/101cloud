<script setup lang="ts">
import type { ErrorText } from '#/locales/errors';

import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';
import { onBeforeRouteLeave, useRouter } from 'vue-router';

import { Page } from '@vben/common-ui';
import { LOGIN_PATH } from '@vben/constants';
import { useAccessStore } from '@vben/stores';
import { Alert, Button, Card, Form, FormItem, InputPassword, message } from 'ant-design-vue';

import { authApi } from '#/api/backend';
import { ApiError, newIdempotencyKey } from '#/api/request';
import ErrorFeedback from '#/components/error-feedback.vue';
import { describeError } from '#/locales/errors';
import { useAuthStore } from '#/store/auth';
import { PASSWORD_HINT, passwordError } from '#/utils/password';

const auth = useAuthStore();
const access = useAccessStore();
const router = useRouter();
const identity = computed(() => auth.me?.user.role === 'user' && !access.loginExpired ? auth.me.user.id : null);
const username = computed(() => auth.me?.user.username ?? '');
const currentPassword = ref('');
const newPassword = ref('');
const confirmation = ref('');
const currentError = ref('');
const feedback = shallowRef<ErrorText | null>(null);
const etag = ref<string | null>(null);
const loading = ref(false);
const submitting = ref(false);
const uncertain = ref(false);
const retryIn = ref(0);
let retryTimer: number | undefined;
let generation = 0;
let disposed = false;
// Credentials remain local to this component and are cleared with its account scope.
let intent: null | { owner: string; body: { current_password: string; new_password: string }; etag: string; key: string } = null;
const newError = computed(() => newPassword.value ? passwordError(newPassword.value) : null);
const confirmationError = computed(() => confirmation.value && confirmation.value !== newPassword.value ? '两次输入的新密码不一致' : '');
const locked = computed(() => submitting.value || uncertain.value);
const valid = computed(() => !!identity.value && !!etag.value && !loading.value && !!currentPassword.value && !!newPassword.value
  && !newError.value && newPassword.value === confirmation.value && retryIn.value === 0 && !locked.value);

function clearCredentials() {
  currentPassword.value = ''; newPassword.value = ''; confirmation.value = '';
  intent = null; uncertain.value = false;
}
async function loadVersion() {
  if (!identity.value || disposed || loading.value || locked.value) return;
  const owner = identity.value;
  const token = generation;
  loading.value = true; etag.value = null; feedback.value = null;
  try {
    const response = await authApi.me();
    if (disposed || token !== generation || identity.value !== owner) return;
    if (response.data.user.id !== owner || response.data.user.role !== 'user' || !response.etag) {
      throw new ApiError(409, { code: 'invalid_response', details: null, message: '账号版本信息不可用，请重新登录' }, response.requestId, null);
    }
    etag.value = response.etag;
  } catch (failure) {
    if (!disposed && token === generation) feedback.value = describeError(failure);
  } finally { if (!disposed && token === generation) loading.value = false; }
}
function startRetryCountdown(seconds: number) {
  window.clearInterval(retryTimer);
  retryIn.value = Math.max(1, Math.ceil(seconds));
  retryTimer = window.setInterval(() => {
    if (--retryIn.value <= 0) window.clearInterval(retryTimer);
  }, 1000);
}
async function submit() {
  if (submitting.value || !identity.value || retryIn.value > 0) return;
  if (!intent) {
    if (!valid.value || !etag.value) return;
    intent = { owner: identity.value, body: { current_password: currentPassword.value, new_password: newPassword.value }, etag: etag.value, key: newIdempotencyKey() };
  }
  const request = intent;
  if (request.owner !== identity.value) return;
  const token = generation;
  const wasUncertain = uncertain.value;
  submitting.value = true; feedback.value = null; currentError.value = '';
  try {
    await authApi.changePassword(request.body, { ifMatch: request.etag, key: request.key });
    if (disposed || token !== generation || identity.value !== request.owner) return;
    clearCredentials();
    // The backend has already revoked every session and cleared the session cookie.
    auth.clear();
    access.setLoginExpired(false);
    message.success('密码已修改，请使用新密码重新登录');
    await router.replace(LOGIN_PATH);
  } catch (failure) {
    if (disposed || token !== generation || identity.value !== request.owner) return;
    const unknown = !(failure instanceof ApiError) || failure.status === 0 || failure.status === 408 || failure.status >= 500
      || (wasUncertain && [401, 403, 429].includes(failure.status));
    uncertain.value = unknown;
    feedback.value = describeError(failure);
    if (failure instanceof ApiError && failure.status === 429) startRetryCountdown(failure.retryAfter ?? 30);
    if (unknown) return;
    intent = null;
    if (failure instanceof ApiError && failure.code === 'invalid_credentials') {
      currentError.value = '当前密码不正确'; currentPassword.value = ''; feedback.value = null;
    } else if (failure instanceof ApiError && [409, 412, 428].includes(failure.status)) {
      etag.value = null;
      feedback.value = { ...describeError(failure), text: '账号信息已变化，请刷新账号版本、核对后重新提交。' };
    }
  } finally { if (!disposed && token === generation) submitting.value = false; }
}
async function returnToLogin() {
  if (submitting.value) return;
  clearCredentials();
  await auth.logout();
}
watch(identity, () => {
  ++generation;
  clearCredentials(); currentError.value = ''; feedback.value = null; etag.value = null;
  submitting.value = false; loading.value = false; retryIn.value = 0;
  window.clearInterval(retryTimer);
  void loadVersion();
}, { immediate: true, flush: 'sync' });
onBeforeRouteLeave(() => {
  if (!identity.value || !locked.value) return true;
  return false;
});
function beforeUnload(event: BeforeUnloadEvent) {
  if (!locked.value) return;
  event.preventDefault(); event.returnValue = '';
}
window.addEventListener('beforeunload', beforeUnload);
onBeforeUnmount(() => {
  disposed = true; ++generation; clearCredentials();
  window.clearInterval(retryTimer); window.removeEventListener('beforeunload', beforeUnload);
});
</script>

<template>
  <Page title="修改密码">
    <Card class="mx-auto w-full max-w-xl" title="修改登录密码">
      <Alert v-if="!identity" type="info" message="请使用普通用户账号登录后修改密码。" show-icon />
      <template v-else>
        <p class="text-muted-foreground mb-4 text-sm">修改平台账号 {{ username }} 的登录密码，不影响实例 root 密码。成功后所有登录会话将退出。</p>
        <Alert v-if="uncertain" class="mb-4" type="warning" show-icon message="提交结果尚未确认，密码输入已锁定。">
          <template #description>请勿重复发起修改。可重试原请求；若会话已失效，请返回登录页尝试新密码。</template>
          <template #action><Button :disabled="submitting" @click="returnToLogin">返回登录</Button></template>
        </Alert>
        <div v-if="feedback" class="mb-4" role="alert"><ErrorFeedback :feedback="feedback" /></div>
        <div v-if="!etag && !uncertain" class="mb-4"><Button :loading="loading" :disabled="submitting" @click="loadVersion">{{ loading ? '正在读取账号版本' : '刷新账号版本' }}</Button></div>
        <Form layout="vertical" @submit.prevent="submit">
          <input :value="username" name="username" autocomplete="username" class="sr-only" readonly tabindex="-1" aria-hidden="true" />
          <FormItem label="当前密码" html-for="member-current-password" required :help="currentError || undefined" :validate-status="currentError ? 'error' : undefined">
            <InputPassword id="member-current-password" v-model:value="currentPassword" name="current_password" autocomplete="current-password" :disabled="locked" :aria-invalid="!!currentError" />
          </FormItem>
          <FormItem label="新密码" html-for="member-new-password" required :help="newError || PASSWORD_HINT" :validate-status="newError ? 'error' : undefined">
            <InputPassword id="member-new-password" v-model:value="newPassword" name="new_password" autocomplete="new-password" :disabled="locked" :aria-invalid="!!newError" />
          </FormItem>
          <FormItem label="确认新密码" html-for="member-confirm-password" required :help="confirmationError || undefined" :validate-status="confirmationError ? 'error' : undefined">
            <InputPassword id="member-confirm-password" v-model:value="confirmation" name="confirm_password" autocomplete="new-password" :disabled="locked" :aria-invalid="!!confirmationError" />
          </FormItem>
          <div class="flex flex-wrap gap-3">
            <Button v-if="uncertain" type="primary" :loading="submitting" :disabled="submitting || retryIn > 0" html-type="submit">{{ retryIn > 0 ? `${retryIn} 秒后重试` : '重试原请求' }}</Button>
            <Button v-else type="primary" html-type="submit" :loading="submitting" :disabled="!valid">{{ retryIn > 0 ? `${retryIn} 秒后重试` : '确认修改密码' }}</Button>
            <Button :disabled="locked" @click="router.push('/instances')">返回实例列表</Button>
          </div>
        </Form>
      </template>
    </Card>
  </Page>
</template>
