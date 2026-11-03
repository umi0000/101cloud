import type {
  CreateElasticSpec,
  CreateLogin,
  CreditAccount,
  ElasticQuote,
  ElasticQuoteSpec,
  Image,
  NetworkOption,
  Plan,
  PolicyVersions,
  Resources,
  TaskAccepted,
} from '#/api/types';
import type { ErrorText } from '#/locales/errors';

import type { LoginDraft, LoginSummary } from '#/utils/instance-login';

import { computed, ref, shallowRef, watch } from 'vue';

import { useAccessStore } from '@vben/stores';

import { defineStore, getActivePinia } from 'pinia';
import type { Pinia } from 'pinia';

import { memberApi } from '#/api/backend';
import { ApiError, newIdempotencyKey } from '#/api/request';
import { describeError } from '#/locales/errors';
import { emptyLoginDraft, imageSupportsLogin, validateLoginDraft } from '#/utils/instance-login';
import { creationResultUncertain } from '#/utils/order-intent';
import { quoteFitsAvailableCredit, quoteHasValidAmounts, sameOrderTerms } from '#/utils/order-terms';

import { useAuthStore } from './auth';
import { useTaskStore } from './tasks';

export interface CreationFormDraft {
  planId?: string;
  networkId?: string;
  imageId?: string;
  name: string;
  resources: Resources;
  gpuCount: number | undefined;
  gpuCap: string;
}

export interface InstanceOrderSelection {
  spec: Omit<CreateElasticSpec, 'login'>;
  plan: Plan;
  image: Image;
  network: NetworkOption;
  quote: ElasticQuote;
  account: CreditAccount | null;
}

type CreateBody = CreateElasticSpec & { policy_versions: PolicyVersions };
interface PendingIntent {
  body: CreateBody;
  key: string;
}

interface LoginDraftAccess {
  read: () => LoginDraft;
  save: (draft: LoginDraft) => void;
}

// This registry contains closures, never reactive Pinia state or persisted snapshots.
const loginDrafts = new WeakMap<Pinia, LoginDraftAccess>();

export function useInstanceLoginDraft(): LoginDraftAccess {
  const pinia = getActivePinia();
  if (!pinia) throw new Error('实例登录草稿需要活动会话');
  useInstanceOrderStore(pinia);
  const access = loginDrafts.get(pinia);
  if (!access) throw new Error('实例登录草稿不可用');
  return access;
}

function quoteSpec(spec: CreateElasticSpec): ElasticQuoteSpec {
  // Explicit allowlist protects quote bodies and public order state from login.
  return {
    gpu: { ...spec.gpu }, image_id: spec.image_id, init_parameters: spec.init_parameters,
    network_option_id: spec.network_option_id, plan_id: spec.plan_id,
    plan_revision: spec.plan_revision, resources: { ...spec.resources },
  };
}

// API DTOs are JSON values. Copy before freezing so callers retain their editable objects.
function snapshot<T>(value: T): T {
  const copy = JSON.parse(JSON.stringify(value)) as T;
  function freeze(item: unknown) {
    if (!item || typeof item !== 'object') return;
    for (const child of Object.values(item)) freeze(child);
    Object.freeze(item);
  }
  freeze(copy);
  return copy;
}

function changedConfiguration(error: unknown): boolean {
  return error instanceof ApiError && ['plan_changed', 'policy_changed'].includes(error.code);
}

export const useInstanceOrderStore = defineStore('instance-order', () => {
  const auth = useAuthStore();
  const access = useAccessStore();
  const tasks = useTaskStore();
  const actor = computed(() => auth.me?.user.role === 'user' ? auth.me.user.id : null);
  const identity = computed(() => !access.loginExpired ? actor.value : null);
  const form = shallowRef<CreationFormDraft | null>(null);
  const order = shallowRef<InstanceOrderSelection | null>(null);
  const quote = shallowRef<ElasticQuote | null>(null);
  const account = shallowRef<CreditAccount | null>(null);
  const accountError = shallowRef<ErrorText | null>(null);
  const quoteError = shallowRef<ErrorText | null>(null);
  const submitError = shallowRef<ErrorText | null>(null);
  const notice = ref('');
  const needsEdit = ref(false);
  const quoteLoading = ref(false);
  const accountLoading = ref(false);
  const submitting = ref(false);
  const hasPendingIntent = ref(false);
  const loginSummary = shallowRef<LoginSummary | null>(null);
  const locked = computed(() => submitting.value || hasPendingIntent.value);
  let pendingIntent: PendingIntent | null = null;
  let loginDraft = emptyLoginDraft();
  let preparedLogin: CreateLogin | null = null;
  const pinia = getActivePinia();
  if (!pinia) throw new Error('实例订单需要活动会话');
  loginDrafts.set(pinia, {
    read: () => identity.value ? { ...loginDraft } : emptyLoginDraft(),
    save: (value) => {
      if (!identity.value || locked.value) return;
      loginDraft = {
        mode: value.mode,
        rootPassword: value.mode === 'password' ? value.rootPassword : '',
        rootPasswordConfirmation: value.mode === 'password' ? value.rootPasswordConfirmation : '',
        sshPublicKeys: value.mode === 'ssh-key' ? value.sshPublicKeys : '',
      };
    },
  });
  let generation = 0;
  let accountSequence = 0;
  let quoteSequence = 0;

  function current(token: number): boolean {
    return token === generation && actor.value !== null;
  }

  function $reset() {
    generation++;
    accountSequence++;
    quoteSequence++;
    form.value = null;
    order.value = null;
    quote.value = null;
    account.value = null;
    accountError.value = null;
    quoteError.value = null;
    submitError.value = null;
    notice.value = '';
    needsEdit.value = false;
    quoteLoading.value = false;
    accountLoading.value = false;
    submitting.value = false;
    pendingIntent = null;
    hasPendingIntent.value = false;
    loginDraft = emptyLoginDraft();
    preparedLogin = null;
    loginSummary.value = null;
  }

  // Preserve an uncertain intent during same-user reauthentication; a different
  // account or explicit logout must still discard every previous user's snapshot.
  watch(actor, $reset, { flush: 'sync' });

  function saveForm(value: CreationFormDraft) {
    if (!identity.value || locked.value) return;
    form.value = {
      planId: value.planId, networkId: value.networkId, imageId: value.imageId,
      name: value.name, resources: { ...value.resources }, gpuCount: value.gpuCount, gpuCap: value.gpuCap,
    };
  }

  async function prepare(value: InstanceOrderSelection): Promise<boolean> {
    if (!identity.value || locked.value) return false;
    const token = generation;
    const savedDraft = { ...loginDraft };
    const validated = await validateLoginDraft(savedDraft);
    if (!current(token) || !identity.value || locked.value) return false;
    if (validated.error || !validated.login || !validated.summary || !imageSupportsLogin(value.image, value.network)) return false;
    const savedForm = form.value;
    const selection = snapshot({
      plan: value.plan, image: value.image, network: value.network, quote: value.quote, account: value.account,
      spec: { ...quoteSpec(value.spec), name: value.spec.name },
    });
    $reset();
    loginDraft = savedDraft;
    preparedLogin = snapshot(validated.login);
    loginSummary.value = snapshot(validated.summary);
    form.value = savedForm;
    order.value = selection;
    quote.value = selection.quote;
    account.value = selection.account;
    if (selection.quote.plan_revision !== selection.spec.plan_revision) requireEdit();
    return true;
  }

  function requireEdit() {
    needsEdit.value = true;
    notice.value = '套餐或政策已变化，请返回配置页重新选择并确认，不能直接提交此配置。';
  }

  function acceptQuote(value: ElasticQuote, previous: ElasticQuote | null) {
    quote.value = snapshot(value);
    quoteError.value = null;
    if (value.plan_revision !== order.value?.spec.plan_revision) {
      requireEdit();
    } else if (previous && !sameOrderTerms(previous, value)) {
      notice.value = '费用、首个计费窗口预留或政策版本已更新，请核对最新报价后再次确认。';
    }
    if (!quoteHasValidAmounts(value)) {
      quoteError.value = { code: 'validation_error', details: [], requestId: '', stage: '', text: '报价金额无效，请重新报价后再确认。' };
    }
  }


  async function refreshAccount(): Promise<void> {
    if (!identity.value || !order.value || submitting.value) return;
    const token = generation;
    const sequence = ++accountSequence;
    accountLoading.value = true;
    try {
      const response = await memberApi.creditAccount();
      if (!current(token) || sequence !== accountSequence) return;
      account.value = snapshot(response.data);
      accountError.value = null;
    } catch (error) {
      if (current(token) && sequence === accountSequence) accountError.value = describeError(error);
    } finally {
      if (current(token) && sequence === accountSequence) accountLoading.value = false;
    }
  }

  async function refreshQuote(): Promise<void> {
    if (!identity.value || !order.value || locked.value || needsEdit.value) return;
    const token = generation;
    const sequence = ++quoteSequence;
    const selection = order.value;
    const previous = quote.value;
    quoteLoading.value = true;
    try {
      const response = await memberApi.quote(quoteSpec(selection.spec), { key: newIdempotencyKey() });
      if (!current(token) || sequence !== quoteSequence) return;
      acceptQuote(response.data, previous);
    } catch (error) {
      if (!current(token) || sequence !== quoteSequence) return;
      quoteError.value = describeError(error);
      if (changedConfiguration(error)) requireEdit();
    } finally {
      if (current(token) && sequence === quoteSequence) quoteLoading.value = false;
    }
  }

  async function submit(): Promise<TaskAccepted | null> {
    if (!identity.value || submitting.value || !order.value) return null;
    const token = generation;
    const selection = order.value;
    let intent = pendingIntent;
    const retrying = intent !== null;
    if (!intent) {
      if (needsEdit.value || quoteLoading.value || accountLoading.value) return null;
      if (!imageSupportsLogin(selection.image, selection.network) || !loginSummary.value) return null;
      const name = selection.spec.name.trim();
      if (!name || Array.from(name).length > 128) {
        submitError.value = {
          code: 'validation_error',
          details: [],
          requestId: '',
          stage: '',
          text: !name ? '请返回配置页填写实例名称。' : '实例名称不能超过 128 个字符，请返回配置页修改。',
        };
        return null;
      }
      const confirmedQuote = quote.value;
      if (!confirmedQuote || quoteError.value) return null;
      // An expired confirmation can only refresh; a second click must confirm the result.
      if (!(Date.parse(confirmedQuote.expires_at) > Date.now())) {
        await refreshQuote();
        return null;
      }
      if (!confirmedQuote.admissible || !quoteHasValidAmounts(confirmedQuote)
        || accountError.value || !account.value || account.value.status !== 'active'
        || !quoteFitsAvailableCredit(confirmedQuote, account.value.available_pt)
        || (account.value.enforcement && account.value.enforcement.status !== 'cancelled')) return null;
    }

    submitting.value = true;
    submitError.value = null;
    try {
      if (!intent) {
        const confirmedQuote = quote.value;
        if (!confirmedQuote) return null;
        const login = preparedLogin;
        if (!identity.value || !login) return null;
        quoteLoading.value = true;
        accountLoading.value = true;
        // Invalidate refreshes started before this final confirmation.
        ++quoteSequence;
        ++accountSequence;
        const [quoted, credited] = await Promise.allSettled([
          memberApi.quote(quoteSpec(selection.spec), { key: newIdempotencyKey() }),
          memberApi.creditAccount(),
        ]);
        if (!current(token)) return null;
        quoteLoading.value = false;
        accountLoading.value = false;
        if (quoted.status === 'fulfilled') {
          acceptQuote(quoted.value.data, confirmedQuote);
        } else {
          quoteError.value = describeError(quoted.reason);
          if (changedConfiguration(quoted.reason)) requireEdit();
        }
        if (credited.status === 'fulfilled') {
          account.value = snapshot(credited.value.data);
          accountError.value = null;
        } else {
          accountError.value = describeError(credited.reason);
        }
        if (quoted.status !== 'fulfilled' || credited.status !== 'fulfilled') return null;
        const checkedQuote = quote.value;
        if (!checkedQuote) return null;
        if (!identity.value || needsEdit.value || !sameOrderTerms(confirmedQuote, checkedQuote)
          || quoteError.value || !checkedQuote.admissible || !(Date.parse(checkedQuote.expires_at) > Date.now())
          || !account.value || account.value.status !== 'active'
          || !quoteFitsAvailableCredit(checkedQuote, account.value.available_pt)
          || (account.value.enforcement && account.value.enforcement.status !== 'cancelled')) return null;
        intent = snapshot({
          body: {
            ...selection.spec,
            name: selection.spec.name.trim(),
            login,
            plan_revision: checkedQuote.plan_revision,
            policy_versions: checkedQuote.policy_versions,
          },
          key: newIdempotencyKey(),
        });
        pendingIntent = intent;
        hasPendingIntent.value = true;
        notice.value = '';
      }

      // An uncertain previous POST always retries this frozen body and key, without requoting.
      const response = await memberApi.create(intent.body, { key: intent.key });
      if (!current(token)) return null;
      tasks.track(response.data.task_id, `创建实例 ${intent.body.name}`);
      $reset();
      return response.data;
    } catch (error) {
      if (!current(token)) return null;
      // Never expose echoed credential input from a transport/server error in state.
      submitError.value = error instanceof ApiError
        ? describeError(new ApiError(error.status, { code: error.code, details: null, message: '' }, error.requestId, error.retryAfter))
        : { code: 'network_error', details: [], requestId: '', stage: '', text: '创建请求未完成，请使用原请求重试。' };
      const uncertain = creationResultUncertain(error instanceof ApiError ? error.status : undefined, retrying);
      if (!uncertain) {
        pendingIntent = null;
        hasPendingIntent.value = false;
        quote.value = null;
        if (changedConfiguration(error)) requireEdit();
      }
      return null;
    } finally {
      // Reset already unlocks its own generation; stale finally blocks cannot touch a new user.
      if (current(token)) {
        submitting.value = false;
        quoteLoading.value = false;
        accountLoading.value = false;
      }
    }
  }

  return {
    $reset,
    account,
    accountError,
    accountLoading,
    form,
    hasPendingIntent,
    loginSummary,
    locked,
    needsEdit,
    notice,
    order,
    prepare,
    quote,
    quoteError,
    quoteLoading,
    refreshAccount,
    refreshQuote,
    saveForm,
    submit,
    submitError,
    submitting,
  };
});
