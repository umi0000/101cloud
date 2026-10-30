<script lang="ts" setup>
import type { NetworkPoolView } from './components/network';

import type { Node, Zone } from '#/api/types';

import { onMounted, ref, shallowRef } from 'vue';
import { useRoute } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Card, TabPane, Tabs } from 'ant-design-vue';

import { catalogApi, listAll, networkApi, nodesApi } from '#/api/backend';
import { errorToast } from '#/composables/api';

import BindingsTab from './components/bindings-tab.vue';
import PoolsTab from './components/pools-tab.vue';

const route = useRoute();
function query(k: string) {
  const v = route.query[k];
  return typeof v === 'string' && v ? v : undefined;
}
const tab = ref(query('tab') === 'bindings' ? 'bindings' : 'pools');

const zones = shallowRef<Zone[]>([]);
const nodes = shallowRef<Node[]>([]);
const pools = shallowRef<NetworkPoolView[]>([]);

/** 下拉选项与名称映射；地址池在切换到绑定标签时重新读取，以包含新登记的池。 */
async function loadPools() {
  try {
    pools.value = await listAll<NetworkPoolView>(networkApi.pools);
  } catch (error) {
    errorToast(error);
  }
}

onMounted(async () => {
  void loadPools();
  try {
    [zones.value, nodes.value] = await Promise.all([listAll(catalogApi.zones), listAll(nodesApi.list)]);
  } catch (error) {
    errorToast(error);
  }
});
</script>

<template>
  <Page description="登记地址池与节点网络绑定。不提供 NAT、端口转发或公网端口配置。" title="网络与 IP">
    <Card size="small">
      <Tabs v-model:active-key="tab" destroy-inactive-tab-pane @change="loadPools">
        <TabPane key="pools" tab="地址池">
          <PoolsTab :zones="zones" />
        </TabPane>
        <TabPane key="bindings" tab="节点网络绑定">
          <BindingsTab
            :initial-node-id="query('node_id')"
            :initial-pool-id="query('pool_id')"
            :nodes="nodes"
            :pools="pools"
          />
        </TabPane>
      </Tabs>
    </Card>
  </Page>
</template>
