<script lang="ts" setup>
/**
 * GPU 策略标签页（仅容器）。管理端所有项可编辑；自定义实例另可设置 allowed_user_actions 与 gpu_user_permissions。
 * PUT 带 If-Match（gpu-policy GET 返回的 ETag 即实例 revision）。
 */
import type { CustomAction, GPUPolicy, GPUPolicyConfig, Instance } from '#/api/types';

import { computed, onMounted, ref, watch } from 'vue';

import {
  Alert,
  Button,
  Card,
  CheckboxGroup,
  Collapse,
  CollapsePanel,
  Descriptions,
  DescriptionsItem,
  Form,
  FormItem,
  Input,
  InputNumber,
  Select,
  Spin,
  Switch,
  Tag,
} from 'ant-design-vue';

import { instancesApi } from '#/api/backend';
import { CUSTOM_ACTIONS, GPU_USER_PERMISSIONS } from '#/api/types';
import ConfirmAction from '#/components/confirm-action.vue';
import TimeText from '#/components/time-text.vue';
import { errorToast, useAction, useResource } from '#/composables/api';
import { CUSTOM_ACTION_NAMES, GPU_PERMISSION_NAMES, POLICY_KIND_NAMES } from '#/locales/enums';
import { formatPt, PT_PATTERN, ptToMicros } from '#/utils/format';

const props = defineProps<{ instance: Instance }>();
const emit = defineEmits<{ changed: [] }>();

const policy = useResource(() => instancesApi.gpuPolicy(props.instance.id));

interface EditState extends GPUPolicyConfig {
  allowed_user_actions: CustomAction[];
  gpu_user_permissions: Record<string, boolean>;
}
const edit = ref<EditState | null>(null);
const dirty = ref(false);

function fill(p: GPUPolicy) {
  edit.value = {
    allowed_user_actions: p.allowed_user_actions.filter((a): a is CustomAction =>
      (CUSTOM_ACTIONS as string[]).includes(a),
    ),
    gpu_user_permissions: Object.fromEntries(GPU_USER_PERMISSIONS.map((k) => [k, p.gpu_user_permissions[k] ?? false])),
    idle: { ...p.idle },
    rebalance: { ...p.rebalance },
  };
  dirty.value = false;
}

async function load() {
  try {
    await policy.reload();
    if (policy.data.value) fill(policy.data.value);
  } catch (error) {
    errorToast(error);
  }
}
onMounted(load);
watch(() => props.instance.revision, () => {
  if (!dirty.value) void load();
});

const save = useAction();
watch(edit, (v, old) => {
  if (old && v === old) {
    dirty.value = true;
    save.resetKey();
  }
}, { deep: true });

const custom = computed(() => props.instance.product_type === 'custom');
const ranges = computed(() => policy.data.value?.allowed_ranges);

const ACTION_NAMES: Record<string, string> = { detach_gpu: '移除 GPU', stop_instance: '停止实例' };
const BLOCKED_NAMES: Record<string, string> = {
  credit_enforcement_active: '存在额度停机事件，暂停自动均衡',
  gpu_requires_container: 'GPU 策略仅支持容器',
  idle_action_not_allowed: '空闲动作不被当前平台政策允许',
  idle_timeout_outside_current_range: '空闲时长超出当前平台范围',
  instance_not_ready: '实例尚未就绪',
  platform_idle_disabled: '平台未启用空闲回收',
};
const SAFETY_NAMES: Record<string, string> = {
  allow_force_stop: '允许强制停止',
  allow_price_increase: '允许换到更贵的卡',
  complete_fresh_ownership_required: '要求完整且新鲜的归属证据',
  cooldown_seconds: '冷却时间（秒）',
  device_references_must_be_zero: '设备引用必须为 0',
  gpu_memory_must_be_zero: '显存占用必须为 0',
  gpu_processes_must_be_zero: 'GPU 进程必须为 0',
  graceful_stop_timeout_seconds: '正常停止超时（秒）',
  metrics_max_age_seconds: '指标最大时效（秒）',
};

const actionOptions = computed(() =>
  (ranges.value?.idle.allowed_actions ?? []).map((a) => ({ label: ACTION_NAMES[a] ?? a, value: a })),
);
const userActionOptions = CUSTOM_ACTIONS.map((a) => ({ label: CUSTOM_ACTION_NAMES[a] ?? a, value: a }));

const problem = computed(() => {
  const e = edit.value;
  const r = ranges.value;
  if (!e || !r) return null;
  const t = e.idle.timeout_seconds;
  if (!Number.isInteger(t) || t < r.idle.min_timeout_seconds || t > r.idle.max_timeout_seconds) {
    return `空闲时长必须在 ${r.idle.min_timeout_seconds}–${r.idle.max_timeout_seconds} 秒之间`;
  }
  if (!r.idle.allowed_actions.includes(e.idle.action)) return '请选择平台允许的空闲动作';
  if (e.idle.enabled && !r.idle.enabled) return '平台未启用空闲回收，不能开启';
  if (!PT_PATTERN.test(e.rebalance.max_hourly_pt)) return '均衡单价上限必须是非负数，最多 6 位小数';
  return null;
});

const priceHint = computed(() => {
  const r = ranges.value?.rebalance;
  const e = edit.value;
  if (!r || !e) return '';
  const v = ptToMicros(e.rebalance.max_hourly_pt);
  const lo = ptToMicros(r.min_hourly_pt);
  const hi = ptToMicros(r.max_hourly_pt);
  const out = v !== null && lo !== null && hi !== null && (v < lo || v > hi);
  return `平台参考范围 ${formatPt(r.min_hourly_pt)}–${formatPt(r.max_hourly_pt)} pt/h${out ? '（当前值超出范围）' : ''}`;
});

const confirmOpen = ref(false);
async function submit() {
  const e = edit.value;
  if (!e || problem.value) return false;
  const body: Parameters<typeof instancesApi.putGPUPolicy>[1] = {
    idle: { action: e.idle.action, enabled: e.idle.enabled, timeout_seconds: e.idle.timeout_seconds },
    rebalance: { allow_live: e.rebalance.allow_live, enabled: e.rebalance.enabled, max_hourly_pt: e.rebalance.max_hourly_pt },
  };
  if (custom.value) {
    body.allowed_user_actions = [...e.allowed_user_actions];
    body.gpu_user_permissions = { ...e.gpu_user_permissions };
  }
  const r = await save.run((key) => instancesApi.putGPUPolicy(props.instance.id, body, { ifMatch: policy.etag.value, key }), {
    onConflict: load,
    success: 'GPU 策略已保存',
  });
  if (!r) return false;
  await load();
  emit('changed');
  return true;
}
</script>

<template>
  <Spin :spinning="policy.loading.value && !policy.data.value">
    <div v-if="policy.data.value && edit && ranges" class="flex flex-col gap-3">
      <Card size="small" title="当前生效">
        <Descriptions :column="2" size="small">
          <DescriptionsItem label="空闲回收">
            {{ policy.data.value.effective_policy.idle.enabled ? '开启' : '关闭' }} ·
            {{ policy.data.value.effective_policy.idle.timeout_seconds }} 秒 ·
            {{ ACTION_NAMES[policy.data.value.effective_policy.idle.action] ?? policy.data.value.effective_policy.idle.action }}
          </DescriptionsItem>
          <DescriptionsItem label="自动均衡">
            {{ policy.data.value.effective_policy.rebalance.enabled ? '开启' : '关闭' }} ·
            {{ policy.data.value.effective_policy.rebalance.allow_live ? '允许运行中换卡' : '不允许运行中换卡' }} ·
            <span class="font-mono">{{ formatPt(policy.data.value.effective_policy.rebalance.max_hourly_pt) }} pt/h</span>
          </DescriptionsItem>
          <DescriptionsItem label="空闲起始"><TimeText :value="policy.data.value.idle_since" /></DescriptionsItem>
          <DescriptionsItem label="上次换卡"><TimeText :value="policy.data.value.last_rebound_at" /></DescriptionsItem>
          <DescriptionsItem label="政策版本">
            <span v-for="(id, kind) in policy.data.value.policy_versions" :key="kind" class="mr-3">
              {{ POLICY_KIND_NAMES[kind] ?? kind }} <span class="font-mono">#{{ id }}</span>
            </span>
          </DescriptionsItem>
          <DescriptionsItem label="未生效原因">
            <template v-if="policy.data.value.blocked_reasons.length > 0">
              <Tag v-for="r in policy.data.value.blocked_reasons" :key="r" class="m-0 mr-1" color="warning">
                {{ BLOCKED_NAMES[r] ?? r }}
              </Tag>
            </template>
            <span v-else>—</span>
          </DescriptionsItem>
        </Descriptions>
        <Collapse class="mt-2" ghost size="small">
          <CollapsePanel key="safety" header="平台安全规则（只读）">
            <Descriptions :column="2" size="small">
              <DescriptionsItem v-for="(v, k) in policy.data.value.effective_policy.safety" :key="k" :label="SAFETY_NAMES[k] ?? k">
                <span class="font-mono">{{ typeof v === 'boolean' ? (v ? '是' : '否') : v }}</span>
              </DescriptionsItem>
            </Descriptions>
          </CollapsePanel>
        </Collapse>
      </Card>

      <Card size="small" title="配置">
        <Form layout="vertical">
          <div class="mb-1 font-medium">空闲回收</div>
          <Alert v-if="!ranges.idle.enabled" class="mb-2" message="平台未启用空闲回收" show-icon type="info" />
          <div class="grid grid-cols-3 gap-x-3">
            <FormItem label="开启">
              <Switch v-model:checked="edit.idle.enabled" :disabled="!ranges.idle.enabled && !edit.idle.enabled" />
            </FormItem>
            <FormItem :help="`${ranges.idle.min_timeout_seconds}–${ranges.idle.max_timeout_seconds} 秒，默认 ${ranges.idle.default_timeout_seconds}`" label="空闲时长（秒）">
              <InputNumber
                v-model:value="edit.idle.timeout_seconds"
                class="w-full"
                :max="ranges.idle.max_timeout_seconds"
                :min="ranges.idle.min_timeout_seconds"
                :precision="0"
              />
            </FormItem>
            <FormItem label="空闲动作">
              <Select v-model:value="edit.idle.action" :options="actionOptions" />
            </FormItem>
          </div>
          <div class="mb-1 font-medium">自动均衡</div>
          <div class="grid grid-cols-3 gap-x-3">
            <FormItem label="开启"><Switch v-model:checked="edit.rebalance.enabled" /></FormItem>
            <FormItem label="允许运行中换卡"><Switch v-model:checked="edit.rebalance.allow_live" /></FormItem>
            <FormItem :help="priceHint" label="均衡单价上限（pt/h）">
              <Input v-model:value="edit.rebalance.max_hourly_pt" class="font-mono" />
            </FormItem>
          </div>
          <template v-if="custom">
            <div class="mb-1 font-medium">用户权限（自定义实例）</div>
            <FormItem label="允许用户执行的操作">
              <CheckboxGroup v-model:value="edit.allowed_user_actions" :options="userActionOptions" />
            </FormItem>
            <FormItem label="允许用户修改的 GPU 策略项">
              <div class="flex flex-wrap gap-x-6 gap-y-2">
                <label v-for="k in GPU_USER_PERMISSIONS" :key="k" class="flex items-center gap-2 text-sm">
                  <Switch v-model:checked="edit.gpu_user_permissions[k]" size="small" />
                  {{ GPU_PERMISSION_NAMES[k] ?? k }}
                </label>
              </div>
            </FormItem>
          </template>
        </Form>
        <Alert v-if="problem" class="mb-2" :message="problem" show-icon type="warning" />
        <div class="flex justify-end gap-2">
          <Button :disabled="!dirty" @click="fill(policy.data.value)">还原</Button>
          <Button :disabled="!!problem || !dirty" type="primary" @click="confirmOpen = true">保存</Button>
        </div>
      </Card>

      <ConfirmAction
        v-model:open="confirmOpen"
        description="提交完整的空闲回收与自动均衡配置。"
        :on-confirm="submit"
        title="保存 GPU 策略"
      />
    </div>
  </Spin>
</template>
