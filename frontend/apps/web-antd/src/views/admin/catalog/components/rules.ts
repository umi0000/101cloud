/**
 * 与后端 backend/internal/catalog 校验一致的前端预检，仅用于提前提示；最终以后端为准。
 */

/** 代码、版本号：字母或数字开头，1–64 位字母、数字、_ . -。 */
export const CODE_PATTERN = /^[A-Za-z0-9][\w.-]{0,63}$/;

export const SHA256_PATTERN = /^[0-9a-fA-F]{64}$/;

/** 脚本最长 256 KiB（按 UTF-8 字节计）。 */
export const SCRIPT_MAX_BYTES = 262_144;

/** 脚本中后端拒绝的片段（不区分大小写）。 */
const SCRIPT_FORBIDDEN = ['-----begin', 'password=', 'password:', 'passwd:', 'private_key', 'token=', 'secret='];

/** 镜像 properties、参数 schema、初始化参数序列化后不得包含的片段（不区分大小写）。 */
const JSON_FORBIDDEN = [
  '-----begin',
  'private_key',
  'privatekey',
  'password',
  'passwd',
  'secret',
  'token',
  'ssh_authorized_keys',
  'user-data',
  'cloud-init',
];

export function scriptForbidden(script: string): string[] {
  const lower = script.toLowerCase();
  return SCRIPT_FORBIDDEN.filter((s) => lower.includes(s));
}

export function jsonForbidden(value: unknown): string[] {
  const lower = JSON.stringify(value).toLowerCase();
  return JSON_FORBIDDEN.filter((s) => lower.includes(s));
}

export function utf8Bytes(text: string): number {
  return new TextEncoder().encode(text).length;
}

export const DEFAULT_SCHEMA = '{\n  "type": "object",\n  "properties": {},\n  "additionalProperties": false\n}';

const SCHEMA_KEYS: Record<string, true> = {
  additionalProperties: true,
  description: true,
  enum: true,
  items: true,
  maximum: true,
  maxItems: true,
  maxLength: true,
  minimum: true,
  minLength: true,
  properties: true,
  required: true,
  title: true,
  type: true,
};
const SCHEMA_TYPES: Record<string, true> = {
  array: true,
  boolean: true,
  integer: true,
  number: true,
  object: true,
  string: true,
};

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** 返回第一个不合规原因；合规返回 null。规则同后端 validateSchemaDefinition。 */
function schemaProblem(s: unknown, path: string, depth: number): null | string {
  if (!isObject(s) || depth > 16) return `${path} 必须是对象（嵌套不超过 16 层）`;
  const unknownKey = Object.keys(s).find((k) => !Object.hasOwn(SCHEMA_KEYS, k));
  if (unknownKey) return `${path} 含不允许的键 "${unknownKey}"`;
  const type = s.type;
  if (typeof type !== 'string' || !Object.hasOwn(SCHEMA_TYPES, type)) {
    return `${path}.type 须为 object/string/integer/number/boolean/array`;
  }
  if (depth === 0 && type !== 'object') return '根节点 type 必须为 object';
  if (type === 'object') {
    const props = s.properties;
    if (!isObject(props)) return `${path} 为 object 时必须有 properties 对象`;
    for (const [k, v] of Object.entries(props)) {
      const p = schemaProblem(v, `${path}.properties.${k}`, depth + 1);
      if (p) return p;
    }
    if (s.additionalProperties !== false) return `${path}.additionalProperties 必须显式为 false`;
    if ('required' in s) {
      const req = s.required;
      if (!Array.isArray(req)) return `${path}.required 必须是字符串数组`;
      const missing = req.find((x) => typeof x !== 'string' || !(x in props));
      if (missing !== undefined) return `${path}.required 中的 "${String(missing)}" 不在 properties 中`;
    }
  }
  if (type === 'array') return schemaProblem(s.items, `${path}.items`, depth + 1);
  return null;
}

/** 解析并校验参数 schema 文本。 */
export function parseSchema(text: string): { error: string } | { value: Record<string, unknown> } {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (error) {
    return { error: `JSON 解析失败：${error instanceof Error ? error.message : String(error)}` };
  }
  if (!isObject(value)) return { error: '参数 schema 必须是 JSON 对象' };
  const problem = schemaProblem(value, 'schema', 0);
  if (problem) return { error: problem };
  const bad = jsonForbidden(value);
  if (bad.length > 0) return { error: `schema 不能包含：${bad.join('、')}（后端会拒绝）` };
  return { value };
}
