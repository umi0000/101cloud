/**
 * 管理端目录引用数据（可用区、镜像、模板、节点、网络绑定、政策版本），
 * 供套餐表单共用：下拉选项与 ID → 名称显示。
 */
import type { Ref } from 'vue';

import type {
  Image,
  InitTemplate,
  NetworkBinding,
  Node,
  PolicyKind,
  PolicyVersion,
  Zone,
} from '#/api/types';

import { reactive, ref } from 'vue';

import { catalogApi, listAll, networkApi, nodesApi, policiesApi } from '#/api/backend';
import { POLICY_KINDS } from '#/api/types';
import { errorToast } from '#/composables/api';
import { enumLabel } from '#/locales/enums';

type Label = (id: null | string | undefined) => string;

export interface CatalogRefs {
  bindingName: Label;
  bindings: Ref<NetworkBinding[]>;
  imageName: Label;
  images: Ref<Image[]>;
  load: () => Promise<void>;
  loading: Ref<boolean>;
  nodeName: Label;
  nodes: Ref<Node[]>;
  policies: Partial<Record<PolicyKind, PolicyVersion[]>>;
  policyName: (kind: PolicyKind, id: null | string | undefined) => string;
  templateName: Label;
  templates: Ref<InitTemplate[]>;
  /** 节点属于该可用区且状态为启用的网络绑定（套餐 network_option_ids 候选）。 */
  zoneBindings: (zoneId: string) => NetworkBinding[];
  zoneName: Label;
  zones: Ref<Zone[]>;
}

export function useCatalogRefs(options: { policies?: boolean } = {}): CatalogRefs {
  const zones = ref<Zone[]>([]);
  const images = ref<Image[]>([]);
  const templates = ref<InitTemplate[]>([]);
  const nodes = ref<Node[]>([]);
  const bindings = ref<NetworkBinding[]>([]);
  const policies = reactive<Partial<Record<PolicyKind, PolicyVersion[]>>>({});
  const loading = ref(false);

  async function load() {
    loading.value = true;
    try {
      const [z, i, t, n, b] = await Promise.all([
        listAll(catalogApi.zones),
        listAll(catalogApi.images),
        listAll(catalogApi.templates),
        listAll(nodesApi.list),
        listAll(networkApi.bindings),
      ]);
      zones.value = z;
      images.value = i;
      templates.value = t;
      nodes.value = n;
      bindings.value = b;
      if (options.policies) {
        const lists = await Promise.all(
          POLICY_KINDS.map((kind) => listAll((q) => policiesApi.list(kind, q))),
        );
        POLICY_KINDS.forEach((kind, idx) => {
          policies[kind] = lists[idx];
        });
      }
    } catch (error) {
      errorToast(error);
    } finally {
      loading.value = false;
    }
  }

  const nodeName: Label = (id) => {
    if (!id) return '—';
    return nodes.value.find((x) => x.id === id)?.name ?? `#${id}`;
  };

  return {
    bindingName: (id) => {
      if (!id) return '—';
      const b = bindings.value.find((x) => x.id === id);
      return b ? `#${b.id} ${enumLabel('networkMode', b.mode).text} ${nodeName(b.node_id)}` : `#${id}`;
    },
    bindings,
    imageName: (id) => {
      if (!id) return '—';
      const i = images.value.find((x) => x.id === id);
      return i ? `${i.name} · ${i.os_name} ${i.os_release}` : `#${id}`;
    },
    images,
    load,
    loading,
    nodeName,
    nodes,
    policies,
    policyName: (kind, id) => {
      if (!id) return '—';
      const p = policies[kind]?.find((x) => x.id === id);
      return p ? `${p.code} v${p.version}` : `#${id}`;
    },
    templateName: (id) => {
      if (!id) return '—';
      const t = templates.value.find((x) => x.id === id);
      return t ? `${t.name}（${t.code} v${t.version}）` : `#${id}`;
    },
    templates,
    zoneBindings: (zoneId) => {
      const inZone = new Set(nodes.value.filter((n) => n.zone_id === zoneId).map((n) => n.id));
      return bindings.value.filter((b) => b.status === 'active' && inZone.has(b.node_id));
    },
    zoneName: (id) => {
      if (!id) return '—';
      const z = zones.value.find((x) => x.id === id);
      return z ? `${z.name}（${z.code}）` : `#${id}`;
    },
    zones,
  };
}
