<script lang="ts" setup>
import { onMounted, ref } from 'vue';
import { useRoute } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Card, TabPane, Tabs } from 'ant-design-vue';

import ImagesTab from './components/images-tab.vue';
import PlansTab from './components/plans-tab.vue';
import TemplatesTab from './components/templates-tab.vue';
import ZonesTab from './components/zones-tab.vue';
import { useCatalogRefs } from './refs';

const route = useRoute();
const tab = ref((route.query.tab as string) || 'zones');
const refs = useCatalogRefs({ policies: true });

onMounted(() => refs.load());
</script>

<template>
  <Page title="产品目录">
    <Card size="small">
      <Tabs v-model:active-key="tab">
        <TabPane key="zones" tab="可用区"><ZonesTab @changed="refs.load" /></TabPane>
        <TabPane key="images" tab="镜像"><ImagesTab @changed="refs.load" /></TabPane>
        <TabPane key="plans" tab="套餐"><PlansTab :refs="refs" /></TabPane>
        <TabPane key="templates" tab="初始化模板"><TemplatesTab @changed="refs.load" /></TabPane>
      </Tabs>
    </Card>
  </Page>
</template>
