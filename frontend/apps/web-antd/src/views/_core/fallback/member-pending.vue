<script lang="ts" setup>
import { Button, Result } from 'ant-design-vue';

import { useAuthStore } from '#/store';

const auth = useAuthStore();
</script>

<template>
  <div class="flex h-full min-h-[60vh] items-center justify-center">
    <Result status="info" sub-title="当前版本只开放管理员面板。成员端（创建实例、额度与账单等）尚未上线。" title="成员端暂未开放">
      <template #extra>
        <Button @click="auth.logout()">退出登录</Button>
      </template>
    </Result>
  </div>
</template>
