<script lang="ts" setup>
import type { ErrorText } from '#/locales/errors';

import { onBeforeUnmount, reactive, ref } from 'vue';
import { useRoute, useRouter } from 'vue-router';

import { preferences } from '@vben/preferences';

import { Alert, Button, Form, FormItem, Input, InputPassword } from 'ant-design-vue';

import { ApiError } from '#/api/request';
import ErrorFeedback from '#/components/error-feedback.vue';
import { describeError } from '#/locales/errors';
import { useAuthStore } from '#/store';

defineOptions({ name: 'Login' });

const props = withDefaults(defineProps<{ embedded?: boolean }>(), {
  embedded: false,
});

const authStore = useAuthStore();
const router = useRouter();
const route = useRoute();

const form = reactive({ password: '', username: '' });
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
    await authStore.login(form.username, form.password);
    form.password = '';
    if (!props.embedded) {
      const redirect = route.query.redirect as string | undefined;
      await router.replace(
        redirect ? decodeURIComponent(redirect) : preferences.app.defaultHomePath,
      );
    }
  } catch (error) {
    if (error instanceof ApiError && error.code === 'rate_limited' && error.retryAfter) {
      startRetryCountdown(error.retryAfter);
    }
    errorFeedback.value = describeError(error);
  }
}

onBeforeUnmount(() => window.clearInterval(timer));
</script>

<template>
  <div class="w-full">
    <div v-if="!embedded" class="mb-8">
      <h2 class="text-foreground mb-2 text-2xl font-semibold">登录 101cloud</h2>
      <p class="text-muted-foreground text-sm">账号由管理员创建</p>
    </div>
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
  </div>
</template>
