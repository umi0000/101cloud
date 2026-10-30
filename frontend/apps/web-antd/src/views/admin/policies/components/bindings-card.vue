<script lang="ts" setup>
/**
 * 平台默认绑定 GET/PUT /admin/platform-policy-bindings。
 * PUT 请求体是 6 个 kind → 版本 id 的完整映射（不包 bindings），If-Match 用 GET 的 ETag。
 * 后端只接受 status=active 且 effective_at 已到的同 kind 版本。
 */
import type { PlatformBindings, PolicyKind, PolicyVersion } from '#/api/types';

import { computed, ref } from 'vue';

import { Alert, Button, Card, Descriptions, DescriptionsItem, Form, FormItem, Modal, Select } from 'ant-design-vue';

import { policiesApi } from '#/api/backend';
import { POLICY_KINDS } from '#/api/types';
import { useAction } from '#/composables/api';
import { POLICY_KIND_NAMES } from '#/locales/enums';

import { versionLabel } from './fields';

const props = defineProps<{
  bindings: null | PlatformBindings;
  etag: null | string;
  loading?: boolean;
  versions: Partial<Record<PolicyKind, PolicyVersion[]>>;
}>();
const emit = defineEmits<{ conflict: []; saved: [] }>();

const editOpen = ref(false);
const draft = ref<Partial<Record<PolicyKind, string>>>({});
const action = useAction();

function label(kind: PolicyKind): string {
  const id = props.bindings?.bindings[kind];
  if (!id) return '未配置';
  const v = props.versions[kind]?.find((x) => x.id === id);
  return v ? versionLabel(v) : `#${id}`;
}

function options(kind: PolicyKind) {
  const now = Date.now();
  return (props.versions[kind] ?? [])
    .filter((v) => v.status === 'active')
    .map((v) => {
      const pending = !v.effective_at || Date.parse(v.effective_at) > now;
      return { disabled: pending, label: `${versionLabel(v)}${pending ? '（尚未生效）' : ''}`, value: v.id };
    });
}

const complete = computed(() => POLICY_KINDS.every((k) => !!draft.value[k]));

function startEdit() {
  draft.value = { ...props.bindings?.bindings };
  action.resetKey();
  editOpen.value = true;
}

async function save() {
  if (!complete.value) return;
  const entries: [PolicyKind, string][] = [];
  for (const k of POLICY_KINDS) {
    const id = draft.value[k];
    if (!id) return;
    entries.push([k, id]);
  }
  const body = Object.fromEntries(entries) as Record<PolicyKind, string>;
  const r = await action.run((key) => policiesApi.putBindings(body, { ifMatch: props.etag, key }), {
    onConflict: () => emit('conflict'),
    success: '平台默认绑定已更新',
  });
  if (!r) return;
  editOpen.value = false;
  emit('saved');
}
</script>

<template>
  <Card :loading="loading && !bindings" size="small" title="平台默认绑定">
    <template #extra>
      <Button size="small" :disabled="!bindings || !etag" @click="startEdit">修改</Button>
    </template>
    <Descriptions :column="{ xs: 1, sm: 2, lg: 3 }" size="small">
      <DescriptionsItem v-for="k in POLICY_KINDS" :key="k" :label="POLICY_KIND_NAMES[k]">
        <span class="font-mono">{{ label(k) }}</span>
      </DescriptionsItem>
    </Descriptions>
    <div class="text-muted-foreground text-xs">新账户使用这里的额度政策；已有账户不会自动切换</div>

    <Modal
      v-model:open="editOpen"
      :confirm-loading="action.pending.value"
      destroy-on-close
      :ok-button-props="{ disabled: !complete }"
      ok-text="保存"
      title="修改平台默认绑定"
      :width="600"
      @ok="save"
    >
      <Alert class="mb-3" message="新账户使用这里的额度政策；已有账户不会自动切换" show-icon type="info" />
      <Form layout="vertical">
        <FormItem v-for="k in POLICY_KINDS" :key="k" :label="POLICY_KIND_NAMES[k]" required>
          <Select
            v-model:value="draft[k]"
            :options="options(k)"
            placeholder="选择已生效的发布版本"
            @change="action.resetKey()"
          />
        </FormItem>
      </Form>
    </Modal>
  </Card>
</template>
