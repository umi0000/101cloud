<script lang="ts" setup>
import type { PolicyVersion, RegistrationSettingsInput } from '#/api/types';

import { computed, onMounted, ref, watch } from 'vue';

import { Alert, Button, Form, FormItem, InputNumber, Select, Skeleton, Space, Switch } from 'ant-design-vue';

import { listAll, policiesApi, registrationSettingsApi } from '#/api/backend';
import { errorToast, useAction, useResource } from '#/composables/api';
import { bytesToGib, gibToBytes } from '#/utils/format';

import { isEffective } from '../../finance/components/credit-policy';

const UINT32_MAX = 4_294_967_295;
type CountField = 'max_gpus' | 'max_instances' | 'max_resident_instances' | 'max_vcpus';
type GibField = 'max_disk_gib' | 'max_memory_gib';
const countFields: { field: CountField; label: string }[] = [
  { field: 'max_instances', label: '最大实例数' },
  { field: 'max_resident_instances', label: '最大自定义（常驻）实例数' },
  { field: 'max_vcpus', label: '最大 vCPU' },
  { field: 'max_gpus', label: '最大 GPU' },
];
const gibFields: { field: GibField; label: string; source: 'max_disk_bytes' | 'max_memory_bytes' }[] = [
  { field: 'max_memory_gib', label: '最大内存（GiB）', source: 'max_memory_bytes' },
  { field: 'max_disk_gib', label: '最大磁盘（GiB）', source: 'max_disk_bytes' },
];
const settings = useResource(registrationSettingsApi.get);
const enabled = ref(false);
const policyId = ref<string | undefined>();
const form = ref<Record<CountField | GibField, number | undefined>>({
  max_disk_gib: undefined, max_gpus: undefined, max_instances: undefined,
  max_memory_gib: undefined, max_resident_instances: undefined, max_vcpus: undefined,
});
const versions = ref<PolicyVersion[]>([]);
const policiesLoading = ref(true);
const policiesFailed = ref(false);
const conflict = ref(false);
const save = useAction();
const policyOptions = computed(() => versions.value.filter((v) => isEffective(v)).map((v) => ({ label: `${v.code} v${v.version}`, value: v.id })));

function fill() {
  const current = settings.data.value;
  if (!current) return;
  enabled.value = current.enabled;
  policyId.value = current.credit_policy_version_id ?? undefined;
  form.value = {
    max_disk_gib: bytesToGib(current.limits.max_disk_bytes),
    max_memory_gib: bytesToGib(current.limits.max_memory_bytes),
    max_gpus: current.limits.max_gpus,
    max_instances: current.limits.max_instances,
    max_resident_instances: current.limits.max_resident_instances,
    max_vcpus: current.limits.max_vcpus,
  };
}
watch(settings.data, fill);
watch([enabled, policyId, form], () => save.resetKey(), { deep: true });

function bytesFor(field: GibField, source: 'max_disk_bytes' | 'max_memory_bytes'): null | string {
  const gib = form.value[field];
  const current = settings.data.value?.limits[source];
  if (typeof gib !== 'number') return null;
  if (current !== undefined && bytesToGib(current) === gib) return current;
  try { return gibToBytes(gib); } catch { return null; }
}

const body = computed<{ error: string } | { value: RegistrationSettingsInput }>(() => {
  const counts: Record<CountField, number> = { max_gpus: 0, max_instances: 0, max_resident_instances: 0, max_vcpus: 0 };
  for (const { field, label } of countFields) {
    const value = form.value[field];
    if (typeof value !== 'number' || !Number.isInteger(value) || value < 0 || value > UINT32_MAX) return { error: `${label}必须是非负整数` };
    counts[field] = value;
  }
  const memory = bytesFor('max_memory_gib', 'max_memory_bytes');
  const disk = bytesFor('max_disk_gib', 'max_disk_bytes');
  if (memory === null || disk === null) return { error: '容量必须是非负数字，最多 3 位小数' };
  if (counts.max_resident_instances > counts.max_instances) return { error: '最大自定义实例数不能大于最大实例数' };
  if (enabled.value && !policyOptions.value.some((p) => p.value === policyId.value)) return { error: '开放注册必须选择已生效的额度政策' };
  return { value: {
    enabled: enabled.value,
    credit_policy_version_id: policyId.value ?? null,
    limits: { ...counts, max_disk_bytes: disk, max_memory_bytes: memory },
  } };
});
const blocked = computed(() => settings.loading.value || !!settings.error.value || !settings.etag.value || policiesLoading.value || policiesFailed.value || conflict.value || 'error' in body.value);

async function loadPolicies() {
  policiesLoading.value = true;
  policiesFailed.value = false;
  try { versions.value = await listAll((q) => policiesApi.list('credit', q)); }
  catch (error) { policiesFailed.value = true; errorToast(error); }
  finally { policiesLoading.value = false; }
}
async function reload() {
  try {
    await settings.reload();
    conflict.value = false;
  } catch (error) { errorToast(error); }
}
async function submit() {
  const b = body.value;
  if (blocked.value || 'error' in b) return;
  const result = await save.run((key) => registrationSettingsApi.put(b.value, { ifMatch: settings.etag.value, key }), {
    onConflict: () => { conflict.value = true; },
    success: '注册设置已保存',
  });
  if (result) await reload();
}
onMounted(() => { void reload(); void loadPolicies(); });
</script>

<template>
  <div>
    <Alert class="mb-4" message="仅影响之后自助注册的用户" description="不会修改已有用户或管理员创建用户的配额。0 表示禁止新增该项资源。自助注册成功即获得所选政策当前业务周期的每日额度（非额外赠送），从注册时生效，按有效期或下一次每日重置时间中较早者到期。业务周期按政策时区和重置时间划分，同一周期只发放一次、消耗后不补满；未用每日额度不结转，兑换及管理员额外发放仍按各自有效期保留。" show-icon type="info" />
    <Alert v-if="settings.error.value" class="mb-4" message="注册设置加载失败，不能保存" show-icon type="error">
      <template #action><Button :loading="settings.loading.value" @click="reload">重试</Button></template>
    </Alert>
    <Skeleton v-if="!settings.data.value && settings.loading.value" active />
    <Form v-if="settings.data.value" layout="vertical" :disabled="save.pending.value || settings.loading.value">
      <FormItem label="开放用户注册" html-for="registration-enabled">
        <Switch id="registration-enabled" v-model:checked="enabled" />
      </FormItem>
      <FormItem label="默认共享钱包额度政策" html-for="registration-policy" :required="enabled">
        <Select id="registration-policy" v-model:value="policyId" allow-clear :disabled="save.pending.value || policiesLoading || policiesFailed" :loading="policiesLoading" option-filter-prop="label" :options="policyOptions" placeholder="请选择已生效的额度政策" show-search />
      </FormItem>
      <Alert v-if="policiesFailed" class="mb-4" message="额度政策加载失败，不能保存" show-icon type="error">
        <template #action><Button @click="loadPolicies">重试</Button></template>
      </Alert>
      <Alert v-else-if="!policiesLoading && !policyOptions.length" class="mb-4" message="暂无已生效的额度政策，请在政策配置中发布后刷新。关闭注册时可不选择政策。" show-icon type="warning">
        <template #action><Button @click="loadPolicies">刷新政策</Button></template>
      </Alert>
      <Alert v-else-if="policyId && !policyOptions.some((p) => p.value === policyId)" class="mb-4" :message="`原额度政策 #${policyId} 已不可选，请选择有效政策或清空后关闭注册。`" show-icon type="warning" />
      <div class="grid grid-cols-1 gap-x-4 sm:grid-cols-2">
        <FormItem v-for="c in countFields" :key="c.field" :html-for="`registration-${c.field}`" :label="c.label">
          <InputNumber :id="`registration-${c.field}`" v-model:value="form[c.field]" class="w-full" :max="UINT32_MAX" :min="0" :step="1" />
        </FormItem>
        <FormItem v-for="g in gibFields" :key="g.field" :html-for="`registration-${g.field}`" :label="g.label">
          <InputNumber :id="`registration-${g.field}`" v-model:value="form[g.field]" class="w-full" :min="0" />
        </FormItem>
      </div>
      <Alert v-if="'error' in body" class="mb-4" :message="body.error" show-icon type="warning" />
      <Alert v-if="conflict" class="mb-4" message="设置已被其他操作修改。请重新加载最新设置并确认后再保存，不会自动覆盖。" show-icon type="warning" />
      <Alert v-if="!settings.etag.value" class="mb-4" message="缺少设置版本，无法安全保存，请重新加载。" show-icon type="error" />
      <Space wrap>
        <Button :disabled="blocked" :loading="save.pending.value" type="primary" @click="submit">保存注册设置</Button>
        <Button :disabled="save.pending.value" :loading="settings.loading.value" @click="reload">重新加载（放弃修改）</Button>
      </Space>
    </Form>
  </div>
</template>
