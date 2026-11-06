<script lang="ts" setup>
import type { ErrorText } from '#/locales/errors';

import { onBeforeUnmount, onMounted, reactive, ref } from 'vue';
import { useRoute, useRouter } from 'vue-router';

import { Alert, Button, Form, FormItem, Input, InputPassword } from 'ant-design-vue';

import { authApi } from '#/api/backend';
import { ApiError } from '#/api/request';
import ErrorFeedback from '#/components/error-feedback.vue';
import { describeError } from '#/locales/errors';
import { useAuthStore } from '#/store';
import { getLoginRedirect } from '#/router/home';

defineOptions({ name: 'Login' });

const props = withDefaults(defineProps<{ embedded?: boolean }>(), {
  embedded: false,
});

const authStore = useAuthStore();
const router = useRouter();
const route = useRoute();

const prefill = typeof route.query.username === 'string' ? route.query.username : '';
const form = reactive({ password: '', username: [...prefill].length <= 64 && prefill.trim() === prefill ? prefill : '' });
const registered = !props.embedded && route.query.registered === '1';
const registrationEnabled = ref(false);
const errorFeedback = ref<ErrorText | null>(null);
const retryIn = ref(0);
let timer: number | undefined;

function startRetryCountdown(seconds: number) {
  retryIn.value = seconds;
  window.clearInterval(timer);
  timer = window.setInterval(() => {
    retryIn.value -= 1;
    if (retryIn.value <= 0) window.clearInterval(timer);
  }, 1000);
}

async function submit() {
  errorFeedback.value = null;
  if (!form.username || !form.password) {
    errorFeedback.value = { code: '', details: [], requestId: '', stage: '', text: '请输入用户名和密码' };
    return;
  }
  try {
    const previousUser = authStore.me?.user;
    await authStore.login(form.username, form.password);
    form.password = '';
    if (authStore.me) {
      const changedUser = previousUser && (previousUser.id !== authStore.me.user.id || previousUser.role !== authStore.me.user.role);
      if (!props.embedded || changedUser) {
        await router.replace({
          path: getLoginRedirect(props.embedded ? undefined : route.query.redirect, authStore.me.user.role),
          force: !!changedUser,
        });
      }
    }
  } catch (error) {
    if (error instanceof ApiError && error.code === 'rate_limited' && error.retryAfter) {
      startRetryCountdown(error.retryAfter);
    }
    errorFeedback.value = describeError(error);
  }
}

onMounted(async () => {
  if (props.embedded) return;
  try {
    registrationEnabled.value = (await authApi.registration()).data.enabled;
  } catch {
    registrationEnabled.value = false;
  }
});

onBeforeUnmount(() => window.clearInterval(timer));
</script>

<template>
  <div class="w-full">
    <div v-if="!embedded" class="mb-8">
      <h2 class="text-foreground mb-2 text-2xl font-semibold">登录 101cloud</h2>
      <p class="text-muted-foreground text-sm">使用用户名和密码登录</p>
    </div>
    <Alert v-if="registered" class="mb-4" message="注册成功，请使用刚设置的密码登录" role="status" show-icon type="success" />
    <Alert v-if="errorFeedback" class="mb-4" :message="errorFeedback.text" show-icon type="error">
      <template #description><ErrorFeedback :feedback="errorFeedback" hide-message /></template>
    </Alert>
    <Form layout="vertical" @submit.prevent="submit">
      <FormItem label="用户名">
        <Input
          v-model:value="form.username"
          autocomplete="username"
          :maxlength="64"
          size="large"
        />
      </FormItem>
      <FormItem label="密码">
        <InputPassword
          v-model:value="form.password"
          autocomplete="current-password"
          size="large"
          @press-enter="submit"
        />
      </FormItem>
      <Button
        block
        :disabled="retryIn > 0"
        html-type="submit"
        :loading="authStore.loginLoading"
        size="large"
        type="primary"
      >
        {{ retryIn > 0 ? `请在 ${retryIn} 秒后重试` : '登录' }}
      </Button>
    </Form>
    <p v-if="registrationEnabled && !embedded" class="mt-4 text-center text-sm">
      还没有账号？<RouterLink class="text-primary" to="/auth/register">注册账号</RouterLink>
    </p>
  </div>
</template>
