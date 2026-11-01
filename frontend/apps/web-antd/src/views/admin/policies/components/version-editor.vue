<script lang="ts" setup>
/**
 * 新建配置档 POST {code, version, config}：创建即生效（status=active）。
 * 不预填任何经济选项；版本默认 "1"。修改已有配置档在详情抽屉中直接进行。
 */
import type { FormState } from './fields';

import type { PolicyKind, PolicyVersion } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Alert, Form, FormItem, Input, Modal } from 'ant-design-vue';

import { policiesApi } from '#/api/backend';
import { useAction } from '#/composables/api';
import { POLICY_KIND_NAMES } from '#/locales/enums';

import ConfigForm from './config-form.vue';
import { configFromState, emptyState, validateState } from './fields';

const props = defineProps<{ gpuModels?: string[]; kind: PolicyKind }>();
const emit = defineEmits<{ created: [version: PolicyVersion] }>();
const open = defineModel<boolean>('open', { default: false });

const code = ref('');
const version = ref('1');
const state = ref<FormState>(emptyState(props.kind));
const submitted = ref(false);
const action = useAction();

watch(open, (v) => {
  if (!v) return;
  submitted.value = false;
  code.value = '';
  version.value = '1';
  state.value = emptyState(props.kind);
  action.resetKey();
});

watch([code, version, state], () => action.resetKey(), { deep: true });

const validation = computed(() => validateState(props.kind, state.value));
const identityErrors = computed(() => {
  const e: Record<string, string> = {};
  if (!code.value || code.value.length > 64 || code.value.trim() !== code.value) e.code = '必填，最多 64 个字符，首尾不能有空格';
  if (!version.value || version.value.length > 64) e.version = '必填，最多 64 个字符';
  return e;
});
const shownErrors = computed(() => (submitted.value ? validation.value.fields : {}));
const valid = computed(
  () =>
    Object.keys(validation.value.fields).length === 0 &&
    validation.value.cross.length === 0 &&
    Object.keys(identityErrors.value).length === 0,
);

async function submit() {
  submitted.value = true;
  if (!valid.value) return;
  const config = configFromState(props.kind, state.value);
  const r = await action.run(
    (key) => policiesApi.create(props.kind, { code: code.value, config, version: version.value }, { key }),
    { success: '已创建' },
  );
  if (!r) return;
  open.value = false;
  emit('created', r.data);
}
</script>

<template>
  <Modal
    v-model:open="open"
    :confirm-loading="action.pending.value"
    destroy-on-close
    ok-text="创建"
    :title="`新建配置档 · ${POLICY_KIND_NAMES[kind]}`"
    :width="720"
    @ok="submit"
  >
    <Form layout="vertical" class="max-h-[65vh] overflow-y-auto pr-2">
      <div class="flex gap-4">
        <FormItem
          class="flex-1"
          :help="submitted ? identityErrors.code : undefined"
          label="编号 code"
          required
          :validate-status="submitted && identityErrors.code ? 'error' : ''"
        >
          <Input v-model:value="code" class="font-mono" :maxlength="64" />
        </FormItem>
        <FormItem
          class="flex-1"
          :help="submitted ? identityErrors.version : undefined"
          label="版本 version"
          required
          :validate-status="submitted && identityErrors.version ? 'error' : ''"
        >
          <Input v-model:value="version" class="font-mono" :maxlength="64" />
        </FormItem>
      </div>
      <ConfigForm v-model:state="state" :errors="shownErrors" :gpu-models="gpuModels" :kind="kind" />
      <Alert
        v-if="submitted && validation.cross.length > 0"
        class="mb-2"
        :message="validation.cross.join('；')"
        show-icon
        type="error"
      />
    </Form>
  </Modal>
</template>
