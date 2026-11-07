// The page for sending hand-written requests. The form is built from one description
// of the request body, which is used both ways: to make the body from the fields, and
// to fill the fields from a body. Anything a field can't hold goes in the "Other fields"
// box nearest to it, so loading and sending a recorded request changes nothing.
"use strict";

const pageRequired = document.querySelector('meta[name="tack-page"]').dataset.required;

function $(id) {
  return document.getElementById(id);
}

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function button(label, className, onClick) {
  const b = el("button", className, label);
  b.type = "button";
  b.addEventListener("click", onClick);
  return b;
}

function toast(text) {
  const t = $("toast");
  t.textContent = text;
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => (t.hidden = true), 4000);
}

async function api(path, options) {
  const res = await fetch(path, { ...options, headers: { ...options?.headers, "X-Tack-Page-Required": pageRequired } });
  if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
  return res;
}

const isObject = (v) => v !== null && typeof v === "object" && !Array.isArray(v);

// Every field reports changes here, so the preview stays current.
let changed = () => {};

// ---- The description of the body ----
//
// Each kind of field is a function from a spec to { node, get, set }. get returns the
// value, or undefined to leave the key out. set takes a value and returns false if this
// field can't hold it, in which case the caller keeps it somewhere else.

const t = (key, label, extra) => ({ kind: "text", key, label, ...extra });
const area = (key, label, extra) => ({ kind: "area", key, label, ...extra });
const num = (key, label, extra) => ({ kind: "number", key, label, ...extra });
const int = (key, label, extra) => ({ kind: "number", key, label, integer: true, ...extra });
const bool = (key, label, extra) => ({ kind: "bool", key, label, ...extra });
const pick = (key, label, options, extra) => ({ kind: "select", key, label, options, ...extra });
const lines = (key, label, extra) => ({ kind: "lines", key, label, ...extra });
const checks = (key, label, options, extra) => ({ kind: "checks", key, label, options, ...extra });
const json = (key, label, extra) => ({ kind: "json", key, label, ...extra });
const map = (key, label, extra) => ({ kind: "map", key, label, ...extra });
const obj = (key, label, fields, extra) => ({ kind: "object", key, label, fields, ...extra });
const list = (key, label, item, extra) => ({ kind: "list", key, label, item, ...extra });
// A value told apart by one key, like a content part's "type".
const variants = (key, label, by, options, extra) => ({ kind: "variants", key, label, by, options, ...extra });

const cacheControl = obj("cache_control", "Cache control", [
  pick("type", "Type", ["ephemeral"]),
  pick("ttl", "Time to live", ["5m", "1h"]),
]);

const partVariants = [
  { name: "text", fields: [area("text", "Text", { rows: 3 }), cacheControl] },
  { name: "image_url", fields: [obj("image_url", "Image", [t("url", "URL or data: URL"), pick("detail", "Detail", ["auto", "low", "high", "original"])], { open: true })] },
  { name: "input_audio", fields: [obj("input_audio", "Audio", [area("data", "Base64 data", { rows: 2 }), t("format", "Format", { placeholder: "wav, mp3, flac…" })], { open: true })] },
  { name: "file", fields: [obj("file", "File", [t("filename", "File name"), area("file_data", "Data: URL or base64", { rows: 2 }), t("file_id", "File ID")], { open: true })] },
  { name: "video_url", fields: [obj("video_url", "Video", [t("url", "URL")], { open: true })] },
];

// Content is a string, or a list of parts.
const content = (textOnly, optional) => ({ kind: "content", key: "content", label: "Content", textOnly, optional });

const toolCall = obj(null, "Tool call", [
  t("id", "ID"),
  pick("type", "Type", ["function"], { initial: "function" }),
  obj("function", "Function", [t("name", "Name"), area("arguments", "Arguments, as the JSON text sent", { rows: 3, mono: true })], { open: true }),
], { open: true, extra: true });

const name = t("name", "Name");
const messageVariants = [
  { name: "system", fields: [content(true), name] },
  { name: "developer", fields: [content(true), name] },
  { name: "user", fields: [content(false), name] },
  {
    name: "assistant",
    fields: [content(false, true), area("reasoning", "Reasoning", { rows: 3 }), list("tool_calls", "Tool calls", toolCall), t("refusal", "Refusal"), name],
  },
  { name: "tool", fields: [t("tool_call_id", "Answers tool call ID"), content(false)] },
];

const toolVariants = [
  {
    name: "function",
    fields: [obj("function", "Function", [
      t("name", "Name"),
      area("description", "Description", { rows: 3 }),
      json("parameters", "Parameters (a JSON Schema)", { rows: 6 }),
      bool("strict", "Strict"),
    ], { open: true, extra: true })],
  },
];

const effort = ["max", "xhigh", "high", "medium", "low", "minimal", "none"];

const body = obj(null, null, [
  {
    group: "Model and routing",
    fields: [
      t("model", "Model"),
      lines("models", "Fallback models, one per line"),
      bool("stream", "Stream"),
      obj("provider", "Provider routing", [
        lines("order", "Order, one per line", { placeholder: "morph/fp8" }),
        bool("allow_fallbacks", "Allow fallbacks"),
        bool("require_parameters", "Require all parameters"),
        pick("data_collection", "Data collection", ["allow", "deny"]),
        bool("zdr", "Zero data retention only"),
        bool("enforce_distillable_text", "Distillable text only"),
        lines("only", "Only these, one per line"),
        lines("ignore", "Not these, one per line"),
        checks("quantizations", "Quantizations", ["int4", "int8", "fp4", "mxfp4", "nvfp4", "fp6", "fp8", "mxfp8", "fp16", "bf16", "fp32", "unknown"]),
        variants("sort", "Sort", null, [
          { name: "price", value: "price" },
          { name: "throughput", value: "throughput" },
          { name: "latency", value: "latency" },
          { name: "object", label: "by and partition", fields: [pick("by", "By", ["price", "throughput", "latency", "exacto"]), pick("partition", "Partition", ["model", "none"])] },
        ]),
        obj("max_price", "Maximum price", [num("prompt", "Prompt, per million"), num("completion", "Completion, per million"), num("request", "Per request"), num("image", "Per image"), num("audio", "Audio")]),
        num("preferred_min_throughput", "Preferred minimum throughput, tokens a second"),
        num("preferred_max_latency", "Preferred maximum latency, seconds"),
      ], { open: true, extra: true }),
      pick("route", "Route (deprecated)", ["fallback", "sort"]),
    ],
  },
  { group: "Messages", fields: [list("messages", null, { kind: "variants", key: null, label: "Message", by: "role", options: messageVariants, custom: true, extra: true, initial: "user" }, { open: true, add: "Add message" })] },
  {
    group: "Tools",
    fields: [
      list("tools", null, { kind: "variants", key: null, label: "Tool", by: "type", options: toolVariants, extra: true }, { add: "Add tool" }),
      variants("tool_choice", "Tool choice", null, [
        { name: "none", value: "none" },
        { name: "auto", value: "auto" },
        { name: "required", value: "required" },
        { name: "function", label: "a named function", fields: [pick("type", "Type", ["function"], { initial: "function" }), obj("function", "Function", [t("name", "Name")], { open: true })] },
      ]),
      bool("parallel_tool_calls", "Parallel tool calls"),
    ],
  },
  {
    group: "Reasoning",
    fields: [
      obj("reasoning", "Reasoning", [pick("effort", "Effort", effort), int("max_tokens", "Max tokens"), bool("exclude", "Leave it out of the reply"), bool("enabled", "Enabled"), pick("summary", "Summary", ["auto", "concise", "detailed"])], { open: true, extra: true }),
      pick("reasoning_effort", "Reasoning effort (shorthand)", effort),
      bool("include_reasoning", "Include reasoning (deprecated)"),
      pick("verbosity", "Verbosity", ["low", "medium", "high", "xhigh", "max"]),
    ],
  },
  {
    group: "Sampling",
    fields: [
      num("temperature", "Temperature, 0 to 2"),
      num("top_p", "Top P, 0 to 1"),
      int("top_k", "Top K"),
      num("top_a", "Top A"),
      num("min_p", "Min P"),
      num("frequency_penalty", "Frequency penalty, -2 to 2"),
      num("presence_penalty", "Presence penalty, -2 to 2"),
      num("repetition_penalty", "Repetition penalty, 0 to 2"),
      int("seed", "Seed"),
      int("max_tokens", "Max tokens"),
      int("max_completion_tokens", "Max completion tokens"),
      lines("stop", "Stop sequences, one per line (up to 4)"),
      map("logit_bias", "Logit bias, token ID to bias", { number: true }),
      bool("logprobs", "Log probabilities"),
      int("top_logprobs", "Top log probabilities, 0 to 20"),
    ],
  },
  {
    group: "Output",
    fields: [
      variants("response_format", "Response format", "type", [
        { name: "text", fields: [] },
        { name: "json_object", fields: [] },
        { name: "json_schema", fields: [obj("json_schema", "JSON schema", [t("name", "Name"), t("description", "Description"), json("schema", "Schema", { rows: 6 }), bool("strict", "Strict")], { open: true, extra: true })] },
        { name: "grammar", fields: [area("grammar", "Grammar", { rows: 4, mono: true })] },
        { name: "python", fields: [] },
      ], { extra: true }),
      obj("prediction", "Predicted output", [pick("type", "Type", ["content"]), area("content", "Content", { rows: 3 })], { extra: true }),
      checks("modalities", "Modalities", ["text", "image", "audio"]),
    ],
  },
  {
    group: "Other",
    fields: [
      t("session_id", "Session ID"),
      t("user", "User"),
      t("prompt_cache_key", "Prompt cache key"),
      obj("prompt_cache_options", "Prompt cache options", [pick("mode", "Mode", ["explicit"]), pick("ttl", "Time to live", ["30m", "12h"])]),
      cacheControl,
      map("metadata", "Metadata"),
      pick("service_tier", "Service tier", ["auto", "default", "fast", "flex", "priority", "scale", "ultrafast"]),
      obj("stream_options", "Stream options", [bool("include_usage", "Include usage (deprecated)")]),
      obj("debug", "Debug", [bool("echo_upstream_body", "Echo the body sent upstream (streaming only)")]),
    ],
  },
], {
  extra: true,
  extraLabel: "Other fields",
  extraHint: "Merged into the body as written. For plugins, trace, image_config, web_search_options, structured_outputs, stop_server_tools_when, deferred_tools, and anything else.",
});

// ---- Field kinds ----

function labelled(spec, control) {
  const node = el("label", "rq-field");
  node.append(el("span", "rq-label", spec.label), control);
  return node;
}

function textField(spec) {
  const multi = spec.kind === "area";
  const input = el(multi ? "textarea" : "input", spec.mono ? "mono" : "");
  if (multi) input.rows = spec.rows ?? 3;
  else input.type = "text";
  if (spec.placeholder) input.placeholder = spec.placeholder;
  input.addEventListener("input", () => changed());
  // Empty means absent. An empty string that was loaded is kept as one.
  let emptyString = false;
  input.addEventListener("input", () => (emptyString = false));
  return {
    node: labelled(spec, input),
    get: () => (input.value !== "" ? input.value : emptyString ? "" : undefined),
    set(v) {
      if (typeof v !== "string") return false;
      input.value = v;
      emptyString = v === "";
      return true;
    },
  };
}

function numberField(spec) {
  const input = el("input");
  input.type = "text";
  input.inputMode = "decimal";
  let raw = null; // a loaded number, kept as written
  input.addEventListener("input", () => {
    raw = null;
    changed();
  });
  return {
    node: labelled(spec, input),
    get() {
      const text = input.value.trim();
      if (text === "") return undefined;
      if (raw !== null && raw.text === text) return raw.value;
      const n = Number(text);
      if (!Number.isFinite(n) || (spec.integer && !Number.isInteger(n))) throw new Error(`${spec.label}: "${text}" isn't ${spec.integer ? "a whole number" : "a number"}`);
      return n;
    },
    set(v) {
      if (typeof v !== "number") return false;
      input.value = String(v);
      raw = { text: input.value, value: v };
      return true;
    },
  };
}

function selectField(spec, options, onPick) {
  const select = el("select");
  for (const [value, label] of [["", "—"], ...options]) {
    const o = el("option", "", label);
    o.value = value;
    select.append(o);
  }
  select.addEventListener("change", () => {
    onPick?.(select.value);
    changed();
  });
  return select;
}

function boolField(spec) {
  const select = selectField(spec, [["true", "true"], ["false", "false"]]);
  return {
    node: labelled(spec, select),
    get: () => (select.value === "" ? undefined : select.value === "true"),
    set(v) {
      if (typeof v !== "boolean") return false;
      select.value = String(v);
      return true;
    },
  };
}

function pickField(spec) {
  const select = selectField(spec, spec.options.map((o) => [o, o]));
  if (spec.initial) select.value = spec.initial;
  return {
    node: labelled(spec, select),
    clear: () => (select.value = ""),
    get: () => (select.value === "" ? undefined : select.value),
    set(v) {
      if (!spec.options.includes(v)) return false;
      select.value = v;
      return true;
    },
  };
}

function linesField(spec) {
  const input = el("textarea");
  input.rows = 2;
  if (spec.placeholder) input.placeholder = spec.placeholder;
  input.addEventListener("input", () => changed());
  return {
    node: labelled(spec, input),
    get() {
      const items = input.value.split("\n").filter((line) => line !== "");
      return items.length ? items : undefined;
    },
    set(v) {
      if (!Array.isArray(v) || v.length === 0 || !v.every((s) => typeof s === "string" && s !== "" && !s.includes("\n"))) return false;
      input.value = v.join("\n");
      return true;
    },
  };
}

function checksField(spec) {
  const node = el("fieldset", "rq-checks");
  node.append(el("legend", "rq-label", spec.label));
  const boxes = spec.options.map((option) => {
    const label = el("label", "rq-check");
    const box = el("input");
    box.type = "checkbox";
    box.value = option;
    box.addEventListener("change", () => changed());
    label.append(box, " " + option);
    node.append(label);
    return box;
  });
  return {
    node,
    get() {
      const on = boxes.filter((b) => b.checked).map((b) => b.value);
      return on.length ? on : undefined;
    },
    set(v) {
      // Only a set of known values in their usual order fits here.
      if (!Array.isArray(v) || v.length === 0) return false;
      const order = v.map((x) => spec.options.indexOf(x));
      if (order.some((i, n) => i < 0 || (n > 0 && i <= order[n - 1]))) return false;
      for (const b of boxes) b.checked = v.includes(b.value);
      return true;
    },
  };
}

function jsonField(spec) {
  const input = el("textarea", "mono");
  input.rows = spec.rows ?? 4;
  if (spec.placeholder) input.placeholder = spec.placeholder;
  input.addEventListener("input", () => changed());
  return {
    node: labelled(spec, input),
    get() {
      if (input.value.trim() === "") return undefined;
      try {
        return JSON.parse(input.value);
      } catch (err) {
        throw new Error(`${spec.label ?? "Other fields"}: ${err.message}`);
      }
    },
    set(v) {
      input.value = JSON.stringify(v, null, 2);
      return true;
    },
    input,
  };
}

function mapField(spec) {
  const node = el("div", "rq-map");
  node.append(el("span", "rq-label", spec.label));
  const rows = el("div", "rq-rows");
  const add = button("Add", "mini", () => {
    addRow("", "");
    changed();
  });
  node.append(rows, add);
  const entries = [];
  function addRow(key, value) {
    const row = el("div", "rq-map-row");
    const k = el("input");
    const v = el("input");
    k.type = v.type = "text";
    k.placeholder = spec.number ? "token ID" : "key";
    v.placeholder = spec.number ? "bias" : "value";
    k.value = key;
    v.value = value;
    k.addEventListener("input", () => changed());
    v.addEventListener("input", () => changed());
    const entry = { k, v };
    row.append(k, v, button("Remove", "mini", () => {
      entries.splice(entries.indexOf(entry), 1);
      row.remove();
      changed();
    }));
    entries.push(entry);
    rows.append(row);
  }
  return {
    node,
    get() {
      if (entries.length === 0) return undefined;
      const out = {};
      for (const { k, v } of entries) {
        if (spec.number) {
          const n = Number(v.value);
          if (v.value.trim() === "" || !Number.isFinite(n)) throw new Error(`${spec.label}: "${v.value}" isn't a number`);
          out[k.value] = n;
        } else out[k.value] = v.value;
      }
      return out;
    },
    set(value) {
      if (!isObject(value) || !Object.values(value).every((x) => typeof x === (spec.number ? "number" : "string"))) return false;
      for (const [k, v] of Object.entries(value)) addRow(k, String(v));
      return true;
    },
  };
}

// An object's fields, in order, plus an "Other fields" box for keys it doesn't know.
function objectField(spec) {
  const node = el(spec.label ? "details" : "div", "rq-object");
  if (spec.label) {
    node.append(el("summary", "", spec.label));
    node.open = Boolean(spec.open);
  }
  const fields = [];
  const addField = (f, into) => {
    const field = make(f);
    fields.push({ spec: f, field });
    into.append(field.node);
  };
  for (const f of spec.fields) {
    if (f.group) {
      const group = el("fieldset", "rq-group");
      group.append(el("legend", "", f.group));
      for (const g of f.fields) addField(g, group);
      node.append(group);
    } else addField(f, node);
  }
  let extra = null;
  if (spec.extra) {
    extra = jsonField({ label: spec.extraLabel ?? "Other fields", rows: spec.extraLabel ? 4 : 2, placeholder: "{}" });
    const box = el("details", "rq-extra");
    box.append(el("summary", "", spec.extraLabel ?? "Other fields"));
    if (spec.extraHint) box.append(el("p", "hint", spec.extraHint));
    box.append(extra.node);
    extra.box = box;
    node.append(box);
  }
  let order = null; // the keys of what was loaded, in order
  const keyed = fields.filter((f) => f.spec.key);
  return {
    node,
    get() {
      const out = {};
      for (const { spec: f, field } of keyed) {
        const v = field.get();
        if (v !== undefined) out[f.key] = v;
      }
      const more = extra?.get();
      if (more !== undefined) {
        if (!isObject(more)) throw new Error(`${spec.extraLabel ?? spec.label ?? "Other fields"} has to be a JSON object`);
        Object.assign(out, more);
      }
      if (order) {
        const rank = (k) => (order.includes(k) ? order.indexOf(k) : order.length);
        const sorted = {};
        for (const k of Object.keys(out).sort((a, b) => rank(a) - rank(b))) sorted[k] = out[k];
        return Object.keys(sorted).length ? sorted : spec.label ? undefined : sorted;
      }
      return Object.keys(out).length || !spec.label ? out : undefined;
    },
    set(value) {
      if (!isObject(value)) return false;
      if (Object.keys(value).length === 0 && spec.label) return false;
      order = Object.keys(value);
      // What's loaded is all there is: a field that starts filled in is emptied.
      for (const { spec: f, field } of keyed) if (!(f.key in value)) field.clear?.();
      const rest = {};
      for (const [k, v] of Object.entries(value)) {
        const f = keyed.find((x) => x.spec.key === k);
        if (!f || !f.field.set(v)) rest[k] = v;
      }
      if (Object.keys(rest).length) {
        if (!extra) return false;
        extra.set(rest);
        extra.box.open = true;
      }
      if (spec.label && node.tagName === "DETAILS") node.open = true;
      return true;
    },
  };
}

// A list of items, each made from the item spec, which can be added and moved.
function listField(spec) {
  const node = el(spec.label ? "details" : "div", "rq-list");
  if (spec.label) {
    node.append(el("summary", "", spec.label));
    node.open = Boolean(spec.open);
  }
  const items = el("div", "rq-items");
  const entries = [];
  let present = false; // an empty list that was loaded is kept as one
  const add = button(spec.add ?? "Add", "mini", () => {
    addItem();
    changed();
  });
  node.append(items, add);
  function renumber() {
    entries.forEach((e, i) => (e.number.textContent = `${i + 1}`));
  }
  function addItem(value) {
    const wrap = el("div", "rq-item");
    const bar = el("div", "rq-item-bar");
    const number = el("span", "rq-number");
    const field = make({ ...spec.item, label: null });
    const entry = { wrap, field, number };
    const move = (by) => {
      const i = entries.indexOf(entry);
      const j = i + by;
      if (j < 0 || j >= entries.length) return;
      entries.splice(i, 1);
      entries.splice(j, 0, entry);
      if (by < 0) items.insertBefore(wrap, entries[j + 1].wrap);
      else items.insertBefore(wrap, entries[j - 1].wrap.nextSibling);
      renumber();
      changed();
    };
    bar.append(number, field.bar ?? "", button("↑", "mini", () => move(-1)), button("↓", "mini", () => move(1)), button("Remove", "mini", () => {
      entries.splice(entries.indexOf(entry), 1);
      wrap.remove();
      renumber();
      changed();
    }));
    wrap.append(bar, field.node);
    entries.push(entry);
    items.append(wrap);
    renumber();
    if (value !== undefined && !field.set(value)) field.setRaw(value);
    return entry;
  }
  return {
    node,
    get() {
      if (entries.length === 0) return present ? [] : undefined;
      return entries.map((e) => e.field.get());
    },
    set(value) {
      if (!Array.isArray(value)) return false;
      present = true;
      for (const v of value) addItem(v);
      if (spec.label) node.open = true;
      return true;
    },
    addItem,
  };
}

// A value told apart by one key (by), or by being one of a few strings (by null). The
// last choice, "as JSON", holds anything else, whole.
function variantsField(spec) {
  const node = el("div", "rq-variants");
  const names = spec.options.map((o) => [o.name, o.label ?? o.name]);
  if (spec.custom) names.push(["custom", "other role"]);
  names.push(["raw", "as JSON"]);
  const select = selectField(spec, names, (name) => show(name));
  if (spec.key === null) select.value = spec.initial ?? spec.options[0].name;
  const custom = spec.custom ? el("input", "rq-custom") : null;
  if (custom) {
    custom.type = "text";
    custom.placeholder = "role";
    custom.hidden = true;
    custom.addEventListener("input", () => changed());
  }
  const bar = el("span", "rq-variant-pick");
  bar.append(select);
  if (custom) bar.append(custom);
  const body = el("div", "rq-variant-body");
  let current = null;
  const built = new Map();
  function variant(name) {
    if (!built.has(name)) {
      let field = null;
      if (name === "raw") field = jsonField({ label: "Whole value as JSON", rows: 4 });
      else if (name === "custom") field = objectField({ fields: [content(true), name_], extra: true });
      else {
        const o = spec.options.find((x) => x.name === name);
        if (o.fields) field = objectField({ fields: o.fields, extra: spec.extra });
      }
      built.set(name, field);
    }
    return built.get(name);
  }
  function show(name) {
    current = name || null;
    if (custom) custom.hidden = name !== "custom";
    body.replaceChildren();
    const field = current && variant(current);
    if (field) body.append(field.node);
  }
  if (spec.key !== null) {
    const top = labelled(spec, bar);
    node.append(top, body);
  } else node.append(body);
  show(select.value);
  const result = {
    node,
    bar: spec.key === null ? bar : null,
    get() {
      if (!current) return undefined;
      if (current === "raw") return variant("raw").get();
      const o = spec.options.find((x) => x.name === current);
      if (o && o.value !== undefined) return o.value;
      const inner = variant(current).get() ?? {};
      if (spec.by === null) return inner;
      const tag = current === "custom" ? custom.value : current;
      return { [spec.by]: tag, ...inner };
    },
    set(value) {
      for (const o of spec.options) {
        if (o.value !== undefined && value === o.value) {
          select.value = o.name;
          show(o.name);
          return true;
        }
      }
      if (!isObject(value)) return false;
      let name;
      let rest = value;
      if (spec.by === null) {
        name = spec.options.find((o) => o.fields)?.name;
      } else {
        const tag = value[spec.by];
        if (typeof tag !== "string") return false;
        name = spec.options.some((o) => o.name === tag) ? tag : spec.custom ? "custom" : null;
        if (!name) return false;
        if (Object.keys(value)[0] !== spec.by) return false; // keep the order as it was
        rest = { ...value };
        delete rest[spec.by];
        if (name === "custom") custom.value = tag;
      }
      if (!name) return false;
      select.value = name;
      show(name);
      const field = variant(name);
      if (Object.keys(rest).length === 0) return true;
      return field.set(rest);
    },
    setRaw(value) {
      select.value = "raw";
      show("raw");
      variant("raw").set(value);
    },
  };
  return result;
}
const name_ = t("name", "Name");

// Content: text, or a list of parts. An assistant's can also be left out or null.
function contentField(spec) {
  const node = el("div", "rq-content");
  const modes = [["text", "text"], ["parts", "parts"]];
  if (spec.optional) modes.push(["null", "null"]);
  const select = selectField(spec, modes, (m) => show(m));
  select.value = "text";
  const text = el("textarea");
  text.rows = 3;
  text.addEventListener("input", () => changed());
  const parts = listField({
    key: "content",
    item: { kind: "variants", key: null, label: "Part", by: "type", options: spec.textOnly ? partVariants.slice(0, 1) : partVariants, extra: true },
    add: "Add part",
  });
  const head = el("div", "rq-field rq-content-head");
  head.append(el("span", "rq-label", "Content"), select);
  node.append(head, text, parts.node);
  function show(mode) {
    text.hidden = mode !== "text";
    parts.node.hidden = mode !== "parts";
  }
  show("text");
  return {
    node,
    get() {
      switch (select.value) {
        case "text":
          return text.value;
        case "parts":
          return parts.get() ?? [];
        case "null":
          return null;
        default:
          return undefined;
      }
    },
    set(v) {
      if (typeof v === "string") {
        select.value = "text";
        text.value = v;
      } else if (Array.isArray(v)) {
        select.value = "parts";
        parts.set(v);
      } else if (v === null && spec.optional) select.value = "null";
      else return false;
      show(select.value);
      return true;
    },
    // Left out entirely, when what was loaded didn't have it.
    clear() {
      select.value = "";
      show("");
    },
  };
}

function make(spec) {
  switch (spec.kind) {
    case "text":
    case "area":
      return textField(spec);
    case "number":
      return numberField(spec);
    case "bool":
      return boolField(spec);
    case "select":
      return pickField(spec);
    case "lines":
      return linesField(spec);
    case "checks":
      return checksField(spec);
    case "json":
      return jsonField(spec);
    case "map":
      return mapField(spec);
    case "object":
      return objectField(spec);
    case "list":
      return listField(spec);
    case "variants":
      return variantsField(spec);
    case "content":
      return contentField(spec);
  }
  throw new Error("unknown field kind " + spec.kind);
}

// ---- The page ----

let form = null;
let loaded = null; // the exact text of what was last loaded, to say if it's unchanged
let defaults = null;
let running = null;

function build(value) {
  changed = () => {};
  form = make(body);
  $("request-form").replaceChildren(form.node);
  if (value !== undefined) {
    if (!form.set(value)) toast("That couldn't be loaded");
  }
  changed = schedulePreview;
  schedulePreview();
}

function blank() {
  const s = defaults?.settings ?? {};
  const value = { model: s.model || "deepseek/deepseek-v4.1-flash-1017", messages: [{ role: "user", content: "" }] };
  if (s.host) value.provider = { order: [s.host], allow_fallbacks: false };
  value.stream = true;
  loaded = null;
  build(value);
}

let previewTimer = null;
function schedulePreview() {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(preview, 120);
  saveDraft();
}

function currentBody() {
  return JSON.stringify(form.get());
}

let pretty = false;
function preview() {
  let text;
  try {
    text = currentBody();
  } catch (err) {
    $("problems").textContent = err.message;
    $("send").disabled = true;
    $("preview").textContent = "";
    $("size").textContent = "";
    return;
  }
  $("problems").textContent = "";
  $("send").disabled = running !== null;
  showJSON($("preview"), text, pretty);
  const bytes = new TextEncoder().encode(text).length;
  const same = loaded !== null && loaded === text ? " · the same bytes as the request it was loaded from" : "";
  $("size").textContent = `${bytes} bytes${same}`;
  countTokens(text, bytes, same);
}

let countSeq = 0;
async function countTokens(text, bytes, same) {
  const seq = ++countSeq;
  try {
    const res = await api("/api/manual-calls/count", { method: "POST", body: text });
    const { tokens } = await res.json();
    if (seq !== countSeq) return;
    $("size").textContent = `${bytes} bytes${tokens ? ` · ${tokens} prompt tokens, counted here` : ""}${same}`;
  } catch {
    // The count is only a convenience.
  }
}

function showJSON(node, text, indent) {
  node.replaceChildren();
  const pieces = indent ? tackJSON.tokens(text) : null;
  if (!pieces) {
    node.textContent = text;
    return;
  }
  for (const p of pieces) node.append(p.type === "space" ? p.text : el("span", "j-" + p.type, p.text));
}

function saveDraft() {
  try {
    localStorage.setItem("tack-request-draft", JSON.stringify({ body: currentBody(), loaded }));
  } catch {
    // A draft is a convenience. A field that doesn't parse yet just isn't saved.
  }
}

function restoreDraft() {
  try {
    const saved = JSON.parse(localStorage.getItem("tack-request-draft"));
    if (saved?.body) {
      loaded = saved.loaded ?? null;
      build(JSON.parse(saved.body));
      return true;
    }
  } catch {
    // Start blank instead.
  }
  return false;
}

async function send() {
  let text;
  try {
    text = currentBody();
  } catch (err) {
    toast(err.message);
    return;
  }
  const result = $("result");
  result.replaceChildren();
  const live = el("div", "rq-result");
  const reasoning = el("pre", "rq-reasoning");
  const reply = el("pre", "rq-text");
  live.append(el("p", "hint", "Sending…"), reasoning, reply);
  result.append(live);
  $("send").disabled = true;
  $("cancel").hidden = false;
  let id = null;
  try {
    const res = await api("/api/manual-calls", { method: "POST", body: text });
    running = "pending";
    const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
    let buffer = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += value;
      let nl;
      while ((nl = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, nl);
        buffer = buffer.slice(nl + 1);
        if (!line) continue;
        const u = JSON.parse(line);
        if (u.kind === "call") {
          id = u.id;
          running = id;
          refreshHistory();
        } else if (u.kind === "reasoning") reasoning.textContent += u.text;
        else if (u.kind === "content") reply.textContent += u.text;
        else if (u.kind === "error") toast(u.text);
      }
    }
  } catch (err) {
    toast(err.message);
  } finally {
    running = null;
    $("cancel").hidden = true;
    preview();
  }
  if (id) await showCall(id);
  refreshHistory();
}

async function cancel() {
  if (!running || running === "pending") return;
  try {
    await api(`/api/manual-calls/${running}/cancel`, { method: "POST" });
  } catch (err) {
    toast(err.message);
  }
}

function usageLine(d) {
  const parts = [];
  if (d.status) parts.push(`HTTP ${d.status}`);
  if (d.host) parts.push(d.host);
  if (d.finish_reason) parts.push(`finished: ${d.finish_reason}`);
  const u = d.usage;
  if (u) {
    if (u.prompt_tokens !== undefined) parts.push(`${u.prompt_tokens} in${u.prompt_tokens_details?.cached_tokens ? ` (${u.prompt_tokens_details.cached_tokens} cached)` : ""}`);
    if (u.completion_tokens !== undefined) parts.push(`${u.completion_tokens} out${u.completion_tokens_details?.reasoning_tokens ? ` (${u.completion_tokens_details.reasoning_tokens} reasoning)` : ""}`);
  }
  if (d.tokens) parts.push(`${d.tokens} counted here`);
  if (d.cost_usd) parts.push(`$${d.cost_usd}`);
  if (d.ttft_ms) parts.push(`first token ${d.ttft_ms} ms`);
  if (d.duration_ms) parts.push(`${d.duration_ms} ms`);
  return parts.join(" · ");
}

function section(label, text, open, isJSON) {
  const box = el("details", "rq-section");
  box.open = open;
  box.append(el("summary", "", label));
  const pre = el("pre", "json");
  if (isJSON) showJSON(pre, text, true);
  else pre.textContent = text;
  box.append(pre);
  return box;
}

function headers(h) {
  return Object.entries(h ?? {}).map(([k, v]) => `${k}: ${v.join(", ")}`).join("\n");
}

async function showCall(id) {
  let d;
  try {
    d = await (await api(`/api/manual-calls/${id}`)).json();
  } catch (err) {
    toast(err.message);
    return;
  }
  const node = el("div", "rq-result");
  const head = el("div", "rq-result-head");
  head.append(el("strong", "", new Date(d.time).toLocaleString()), el("span", "hint", usageLine(d)));
  node.append(head);
  if (d.running) node.append(el("p", "hint", "Still running."));
  if (d.interrupted) node.append(el("p", "rq-error", "tack stopped before this finished. Whatever arrived is below."));
  if (d.error) node.append(el("p", "rq-error", d.error));
  if (d.reasoning) node.append(section("Reasoning", d.reasoning, true));
  if (d.text) node.append(section("Text", d.text, true));
  if (d.tool_calls?.length) node.append(section("Tool calls", d.tool_calls.map((c) => `${c.name} (${c.id})\n${c.arguments}`).join("\n\n"), true));
  if (d.response) node.append(section(`Raw response, ${d.response.length} characters`, d.response, false));
  if (d.response_headers) node.append(section("Response headers", headers(d.response_headers), false));
  node.append(section("Request headers, without the key", headers(d.request_headers), false));
  node.append(section("Request", d.request, false, true));
  node.append(button("Load this request into the form", "mini", () => load(d.request)));
  $("result").replaceChildren(node);
}

function load(text) {
  try {
    const value = JSON.parse(text);
    loaded = text;
    build(value);
    window.scrollTo(0, 0);
  } catch (err) {
    toast("That request isn't JSON: " + err.message);
  }
}

async function refreshHistory() {
  let calls;
  try {
    calls = await (await api("/api/manual-calls")).json();
  } catch {
    return;
  }
  const nav = $("history");
  nav.replaceChildren();
  if (calls.length === 0) nav.append(el("p", "hint", "Nothing yet."));
  for (const c of calls) {
    const b = button("", "conv", () => showCall(c.id));
    const state = c.running ? "running" : c.interrupted ? "interrupted" : c.error ? "error" : c.finish_reason || (c.status ? `HTTP ${c.status}` : "");
    b.append(el("span", "conv-title", `${c.model}${c.host ? " · " + c.host : ""}`), el("span", "conv-when", `${new Date(c.time).toLocaleString()} · ${state}${c.cost_usd ? ` · $${c.cost_usd}` : ""}`));
    nav.append(b);
  }
}

async function refreshRecorded() {
  const select = $("recorded");
  try {
    const calls = await (await api("/api/recorded-calls")).json();
    select.replaceChildren();
    const none = el("option", "", calls.length ? "Choose one…" : "None yet");
    none.value = "";
    select.append(none);
    for (const c of calls) {
      const o = el("option", "", `${new Date(c.time).toLocaleString()} · ${c.conversation ? c.preview || c.conversation : "sent from here"} · ${c.request_bytes} bytes`);
      o.value = c.call;
      select.append(o);
    }
  } catch (err) {
    select.replaceChildren(el("option", "", "Couldn't load them"));
  }
}

async function loadRecorded() {
  const call = $("recorded").value;
  if (!call) return;
  try {
    load(await (await api(`/api/recorded-calls/${call}`)).text());
  } catch (err) {
    toast(err.message);
  }
}

async function start() {
  try {
    defaults = await (await api("/api/defaults")).json();
  } catch {
    defaults = null;
  }
  if (!restoreDraft()) blank();
  $("blank").addEventListener("click", blank);
  $("send").addEventListener("click", send);
  $("cancel").addEventListener("click", cancel);
  $("load-recorded").addEventListener("click", loadRecorded);
  $("pretty").addEventListener("click", () => {
    pretty = !pretty;
    $("pretty").setAttribute("aria-pressed", String(pretty));
    preview();
  });
  $("copy").addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(currentBody());
      toast("Copied");
    } catch (err) {
      toast(err.message);
    }
  });
  refreshHistory();
  refreshRecorded();
}

start();
