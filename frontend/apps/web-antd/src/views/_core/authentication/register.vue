<script lang="ts" setup>
import type { ErrorText } from '#/locales/errors';

import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue';
import { useRouter } from 'vue-router';

import { Alert, Button, Form, FormItem, Input, InputPassword, Skeleton } from 'ant-design-vue';

import { authApi } from '#/api/backend';
import { ApiError } from '#/api/request';
import ErrorFeedback from '#/components/error-feedback.vue';
import { describeError } from '#/locales/errors';
import { passwordError } from '#/utils/password';

const router = useRouter();
const enabled = ref(false);
const loading = ref(true);
const statusError = ref<ErrorText | null>(null);
const feedback = ref<ErrorText | null>(null);
const pending = ref(false);
const attempted = ref(false);
const retryIn = ref(0);
let timer: number | undefined;
const form = reactive({ confirm: '', email: '', password: '', username: '' });
const errors = computed(() => ({
  username: [...form.username].length < 1 || [...form.username].length > 64
    ? '用户名需 1–64 个字符' : form.username.trim() !== form.username ? '用户名首尾不能有空格' : null,
  email: [...form.email.trim()].length > 254 ? '邮箱最多 254 个字符' : null,
  password: passwordError(form.password),
  confirm: !form.confirm || form.confirm !== form.password ? '两次输入的密码需一致' : null,
}));

async function loadStatus() {
  loading.value = true;
  enabled.value = false;
  statusError.value = null;
  feedback.value = null;
  try {
    enabled.value = (await authApi.registration()).data.enabled;
  } catch (error) {
    statusError.value = describeError(error);
  } finally {
    loading.value = false;
  }
}

async function submit() {
  if (pending.value || !enabled.value || loading.value || retryIn.value > 0) return;
  attempted.value = true;
  if (Object.values(errors.value).some(Boolean)) return;
  feedback.value = null;
  pending.value = true;
  try {
    const result = await authApi.register({
      email: form.email.trim() || null,
      password: form.password,
      username: form.username,
    });
    form.password = '';
    form.confirm = '';
    await router.replace({ path: '/auth/login', query: { registered: '1', username: result.data.username } });
  } catch (error) {
    feedback.value = describeError(error);
    if (error instanceof ApiError) {
      if (error.code === 'registration_disabled') enabled.value = false;
      if (error.code === 'rate_limited' && error.retryAfter) {
        retryIn.value = error.retryAfter;
        window.clearInterval(timer);
        timer = window.setInterval(() => {
          retryIn.value -= 1;
          if (retryIn.value <= 0) window.clearInterval(timer);
        }, 1000);
      }
    }
  } finally {
    pending.value = false;
  }
}

onMounted(loadStatus);
onBeforeUnmount(() => {
  window.clearInterval(timer);
  form.password = '';
  form.confirm = '';
});
</script>

<template>
  <div class="w-full">
    <div class="mb-6">
      <h2 class="text-foreground mb-2 text-2xl font-semibold">注册 101cloud</h2>
      <p class="text-muted-foreground text-sm">创建账号后，请使用用户名和密码登录。</p>
    </div>
    <Skeleton v-if="loading" active aria-label="正在检查注册状态" />
    <Alert v-else-if="statusError" class="mb-4" message="无法获取注册状态" show-icon type="error">
      <template #description><ErrorFeedback :feedback="statusError" /></template>
      <template #action><Button @click="loadStatus">重试</Button></template>
    </Alert>
    <Alert v-else-if="!enabled && !feedback" class="mb-4" message="当前未开放注册，请联系管理员" show-icon type="info">
      <template #action><Button @click="loadStatus">刷新状态</Button></template>
    </Alert>
    <Alert v-if="feedback" class="mb-4" :message="feedback.text" role="alert" show-icon type="error">
      <template #description><ErrorFeedback :feedback="feedback" hide-message /></template>
      <template v-if="!enabled" #action><Button @click="loadStatus">刷新状态</Button></template>
    </Alert>
    <Form v-if="enabled && !loading && !statusError" layout="vertical" @submit.prevent="submit">
      <FormItem label="用户名" html-for="register-username" required :help="attempted || form.username ? errors.username : undefined" :validate-status="(attempted || form.username) && errors.username ? 'error' : undefined">
        <Input id="register-username" v-model:value="form.username" autocomplete="username" :disabled="pending" size="large" />
      </FormItem>
      <FormItem label="邮箱（可选）" html-for="register-email" :help="errors.email" :validate-status="errors.email ? 'error' : undefined">
        <Input id="register-email" v-model:value="form.email" autocomplete="email" :disabled="pending" size="large" />
      </FormItem>
      <FormItem label="密码" html-for="register-password" required :help="(attempted || form.password ? errors.password : null) ?? '12–128 个 Unicode 字符'" :validate-status="(attempted || form.password) && errors.password ? 'error' : undefined">
        <InputPassword id="register-password" v-model:value="form.password" autocomplete="new-password" :disabled="pending" size="large" />
      </FormItem>
      <FormItem label="确认密码" html-for="register-confirm" required :help="attempted || form.confirm ? errors.confirm : undefined" :validate-status="(attempted || form.confirm) && errors.confirm ? 'error' : undefined">
        <InputPassword id="register-confirm" v-model:value="form.confirm" autocomplete="new-password" :disabled="pending" size="large" />
      </FormItem>
      <Button block :disabled="retryIn > 0" html-type="submit" :loading="pending" size="large" type="primary">
        {{ retryIn > 0 ? `请在 ${retryIn} 秒后重试` : '注册账号' }}
      </Button>
    </Form>
    <p class="mt-4 text-center text-sm"><RouterLink class="text-primary" to="/auth/login">返回登录</RouterLink></p>
  </div>
</template>
