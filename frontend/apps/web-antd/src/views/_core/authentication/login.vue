<script lang="ts" setup>
import { onBeforeUnmount, reactive, ref } from 'vue';
import { useRoute, useRouter } from 'vue-router';

import { preferences } from '@vben/preferences';

import { Alert, Button, Form, FormItem, Input, InputPassword } from 'ant-design-vue';

import { ApiError } from '#/api/request';
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
const errorText = ref('');
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
  errorText.value = '';
  if (!form.username || !form.password) {
    errorText.value = '请输入用户名和密码';
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
    const d = describeError(error);
    errorText.value = d.requestId ? `${d.text}（${d.requestId}）` : d.text;
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
    <Alert v-if="errorText" class="mb-4" :message="errorText" show-icon type="error" />
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
