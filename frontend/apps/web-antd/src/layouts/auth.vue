<script lang="ts" setup>
import { computed } from 'vue';

import { AuthPageLayout } from '@vben/layouts';
import { preferences } from '@vben/preferences';

const appName = computed(() => preferences.app.name);
</script>

<template>
  <AuthPageLayout
    :app-name="appName"
    logo=""
    page-description="工作室云主机与 GPU 容器管理面板"
    page-title="101cloud"
    :toolbar="false"
  />
</template>
