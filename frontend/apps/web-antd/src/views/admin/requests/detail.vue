<script lang="ts" setup>
import type { CustomRequest, Limits } from '#/api/types';

import { computed, onMounted, ref, shallowRef } from 'vue';
import { useRoute } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Alert, Button, Card, Descriptions, DescriptionsItem, Empty, Spin } from 'ant-design-vue';

import { requestsApi, usersApi } from '#/api/backend';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { errorToast, useResource } from '#/composables/api';

import { useCatalogRefs } from '../catalog/refs';
import ApproveForm from './components/approve-form.vue';
import { diffKeys } from './components/config';
import ConfigView from './components/config-view.vue';

const route = useRoute();
const id = computed(() => String(route.params.id));
const detail = useResource(() => requestsApi.get(id.value));
const refs = useCatalogRefs();
const limits = shallowRef<Limits | null>(null);
/** 待审批时由审批表单回传的差异；已批准时比较申请与批准配置。 */
const formDiff = ref<Set<string>>(new Set());

const req = computed<CustomRequest | null>(() => detail.data.value);
const diff = computed(() => {
  const r = req.value;
  if (r?.status === 'pending') return formDiff.value;
  return r?.approved_config ? diffKeys(r.requested_config, r.approved_config) : new Set<string>();
});

async function load() {
  try {
    await detail.reload();
  } catch (error) {
    errorToast(error);
    return;
  }
  const userId = detail.data.value?.user_id;
  if (userId && !limits.value) {
    usersApi.limits(userId).then(
      (r) => (limits.value = r.data),
      () => (limits.value = null),
    );
  }
}

onMounted(() => {
  void refs.load();
  void load();
});
</script>

<template>
  <Page :title="req ? `申请：${req.name}` : '申请详情'">
    <template #extra>
      <Button @click="load">刷新</Button>
    </template>
    <Spin :spinning="detail.loading.value && !req">
      <Empty v-if="!req && !detail.loading.value" description="申请不存在或无法读取" />
      <div v-if="req" class="grid grid-cols-1 gap-3 xl:grid-cols-2">
        <Card size="small" title="成员申请">
          <Descriptions bordered class="mb-3" :column="1" size="small">
            <DescriptionsItem label="ID"><span class="font-mono">{{ req.id }}</span></DescriptionsItem>
            <DescriptionsItem label="申请人"><UserName :id="req.user_id" link /></DescriptionsItem>
            <DescriptionsItem label="状态"><StatusTag kind="requestStatus" :value="req.status" /></DescriptionsItem>
            <DescriptionsItem label="提交时间"><TimeText :value="req.created_at" /></DescriptionsItem>
            <DescriptionsItem label="申请理由"><div class="whitespace-pre-wrap">{{ req.reason }}</div></DescriptionsItem>
            <DescriptionsItem v-if="limits" label="成员配额">
              实例数上限 <span class="font-mono">{{ limits.max_instances }}</span>，常驻实例上限
              <span class="font-mono">{{ limits.max_resident_instances }}</span>
            </DescriptionsItem>
          </Descriptions>
          <div class="text-muted-foreground mb-2 text-xs">申请配置{{ diff.size > 0 ? '（高亮为批准时修改的字段）' : '' }}</div>
          <ConfigView :config="req.requested_config" :diff="diff" :refs="refs" />
        </Card>

        <Card v-if="req.status === 'pending'" size="small" title="批准配置">
          <ApproveForm
            v-model:diff="formDiff"
            :etag="detail.etag.value"
            :refs="refs"
            :request="req"
            @conflict="load"
            @done="load"
          />
        </Card>
        <Card v-else size="small" title="审批结果">
          <Descriptions bordered class="mb-3" :column="1" size="small">
            <DescriptionsItem label="审批人"><UserName :id="req.reviewed_by_user_id" link /></DescriptionsItem>
            <DescriptionsItem label="审批时间"><TimeText :value="req.reviewed_at" /></DescriptionsItem>
            <DescriptionsItem label="审批意见">
              <div class="whitespace-pre-wrap">{{ req.review_note ?? '—' }}</div>
            </DescriptionsItem>
            <DescriptionsItem label="实例">
              <RouterLink v-if="req.instance_id" :to="`/admin/instances/${req.instance_id}`">#{{ req.instance_id }}</RouterLink>
              <span v-else>—</span>
            </DescriptionsItem>
            <DescriptionsItem label="交付任务">
              <RouterLink v-if="req.delivery_task_id" :to="{ path: '/admin/tasks', query: { instance_id: req.instance_id ?? undefined } }">
                #{{ req.delivery_task_id }}
              </RouterLink>
              <span v-else>—</span>
            </DescriptionsItem>
          </Descriptions>
          <template v-if="req.approved_config">
            <div class="text-muted-foreground mb-2 text-xs">批准配置</div>
            <ConfigView :config="req.approved_config" :diff="diff" :refs="refs" />
          </template>
          <Alert v-else message="该申请未被批准，没有批准配置。" type="info" />
        </Card>
      </div>
    </Spin>
  </Page>
</template>
