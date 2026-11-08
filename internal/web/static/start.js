"use strict";

// The start panel: a new conversation's system prompt, as sections of paragraphs, and its
// tools. What's here is exactly what gets sent, apart from {{date}}, which tack fills in
// when the conversation starts. The same tool editor is used for a conversation's tools.

let start = null;
let builtinTools = null; // tack's own tools, as /api/defaults lists them

const PARAGRAPH = "\n\n";

// canonical is JSON with keys in order, for comparing definitions the way tack does.
function canonical(value) {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value).sort().map((k) => `${JSON.stringify(k)}:${canonical(value[k])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

// withoutDescriptions is what of a built-in tool's definition can't be changed.
function withoutDescriptions(definition) {
  const copy = structuredClone(definition);
  delete copy.function?.description;
  for (const property of Object.values(copy.function?.parameters?.properties ?? {})) {
    if (property && typeof property === "object") delete property.description;
  }
  return canonical(copy);
}

function blankTool(tools) {
  let name = "new_tool";
  for (let n = 2; tools.some((t) => t.name === name); n++) name = `new_tool_${n}`;
  return {
    name,
    custom: true,
    definition: { type: "function", function: { name, description: "", parameters: { type: "object", properties: {}, required: [] } } },
  };
}

// toolList is the tools in order, each built-in or custom, offered or not. Built-ins that
// a saved list doesn't mention come last, not offered, so they can still be turned on.
function toolList(saved, notes) {
  const builtins = builtinTools ?? [];
  const fresh = (b, off) => ({ name: b.name, off, definition: structuredClone(b.definition) });
  if (!saved) return builtins.map((b) => fresh(b, false));
  const out = [];
  for (const tool of saved) {
    if (tool.custom) {
      out.push({ name: tool.name, custom: true, off: Boolean(tool.off), definition: structuredClone(tool.definition ?? blankTool(out).definition) });
      continue;
    }
    const b = builtins.find((x) => x.name === tool.name);
    if (!b) {
      notes.push(`${tool.name} was saved, but tack doesn't have it any more, so it's left out.`);
      continue;
    }
    const entry = fresh(b, Boolean(tool.off));
    if (tool.definition && withoutDescriptions(tool.definition) === withoutDescriptions(b.definition)) {
      entry.definition = structuredClone(tool.definition);
    } else if (tool.definition) {
      notes.push(`tack's definition of ${tool.name} changed since it was saved, so its descriptions are tack's again.`);
    }
    out.push(entry);
  }
  for (const b of builtins) {
    if (!out.some((t) => !t.custom && t.name === b.name)) out.push(fresh(b, true));
  }
  return out;
}

// offers is what a conversation is asked to offer: the tools that are on, in order.
function offers(tools) {
  return tools.filter((t) => !t.off).map((t) => (t.custom ? { name: t.name, custom: true, definition: t.definition } : { name: t.name, definition: t.definition }));
}

// Boxes fit their text. Where the browser can do that itself, it does it better, since it
// keeps up when the width changes.
const SIZES_ITSELF = CSS.supports("field-sizing", "content");

function grow(area) {
  if (SIZES_ITSELF) return;
  area.style.height = "auto";
  area.style.height = `${area.scrollHeight + 2}px`;
}

function growing(className, value, label, onInput) {
  const area = el("textarea", className);
  area.value = value ?? "";
  area.rows = 1;
  area.spellcheck = false;
  area.setAttribute("aria-label", label);
  area.addEventListener("input", () => {
    grow(area);
    onInput(area.value);
  });
  requestAnimationFrame(() => grow(area));
  return area;
}

function smallButton(label, title, onClick) {
  const b = button(label, "mini icon-ish", onClick);
  b.title = title;
  b.setAttribute("aria-label", title);
  return b;
}

function moveIn(list, i, by) {
  const j = i + by;
  if (j < 0 || j >= list.length) return false;
  [list[i], list[j]] = [list[j], list[i]];
  return true;
}

function setDescription(holder, value) {
  if (value === "") delete holder.description;
  else holder.description = value;
}

// A line that says what a property is, apart from its description.
function propertySummary(name, property, required) {
  const rest = { ...property };
  delete rest.description;
  const bits = [rest.type ?? "any type"];
  delete rest.type;
  if (rest.enum) bits.push(`one of ${rest.enum.map((v) => JSON.stringify(v)).join(", ")}`);
  delete rest.enum;
  bits.push(required ? "required" : "optional");
  if (Object.keys(rest).length) bits.push(JSON.stringify(rest));
  return bits.join(", ");
}

// toolsEditor edits a list of tools in place. onChange runs after every change.
function toolsEditor(tools, onChange) {
  const node = el("div", "tools-editor");
  const draw = () => {
    node.replaceChildren(allToggle("tool", tools, draw, onChange), ...tools.map((tool, i) => toolEditor(tools, i, draw, onChange)));
    const add = button("+ Custom tool", "mini", () => {
      tools.push(blankTool(tools));
      draw();
      onChange();
      node.querySelector(".tool-editor:last-of-type input[data-name]")?.select();
    });
    add.title = "A tool tack doesn't have. You write the result of every call to it.";
    node.append(add);
  };
  draw();
  node.redraw = draw;
  return node;
}

function toolEditor(tools, i, redraw, onChange) {
  const tool = tools[i];
  const fn = (tool.definition.function ??= {});
  const node = el("div", `tool-editor${tool.off ? " is-off" : ""}${tool.custom ? " is-custom" : ""}`);
  node.dataset.tool = tool.name;
  const head = el("div", "tool-editor-head");
  const on = el("input");
  on.type = "checkbox";
  on.checked = !tool.off;
  on.dataset.offer = tool.name;
  on.setAttribute("aria-label", `Offer ${tool.name}`);
  on.addEventListener("change", () => {
    tool.off = !on.checked;
    node.classList.toggle("is-off", tool.off);
    onChange();
  });
  head.append(on);
  if (tool.custom) {
    const name = el("input", "tool-name-input");
    name.type = "text";
    name.value = tool.name;
    name.dataset.name = "";
    name.spellcheck = false;
    name.setAttribute("aria-label", "Tool name");
    name.addEventListener("input", () => {
      tool.name = fn.name = name.value;
      node.dataset.tool = tool.name;
      onChange();
    });
    head.append(name, el("span", "tag", "custom"));
  } else {
    head.append(exact(tool.name, "tool-editor-name"));
  }
  head.append(el("span", "spacer"));
  head.append(
    smallButton("↑", "Move up", () => moveIn(tools, i, -1) && (redraw(), onChange())),
    smallButton("↓", "Move down", () => moveIn(tools, i, 1) && (redraw(), onChange())),
  );
  if (tool.custom) {
    head.append(smallButton("✕", "Remove this tool", () => {
      tools.splice(i, 1);
      redraw();
      onChange();
    }));
  }
  node.append(head);
  const description = growing("tool-description", fn.description, `${tool.name} description`, (value) => {
    setDescription(fn, value);
    onChange();
  });
  description.dataset.description = tool.name;
  description.placeholder = "No description";
  node.append(description);

  if (tool.custom) {
    const schema = growing("tool-schema", JSON.stringify(fn.parameters ?? {}, null, 2), `${tool.name} parameters`, (value) => {
      try {
        fn.parameters = JSON.parse(value);
        schema.classList.remove("bad");
      } catch {
        schema.classList.add("bad");
        return;
      }
      onChange();
    });
    schema.dataset.schema = tool.name;
    node.append(el("div", "hint", "Parameters, as JSON Schema"), schema);
  } else {
    const parameters = fn.parameters ?? {};
    const required = parameters.required ?? [];
    for (const [name, property] of Object.entries(parameters.properties ?? {})) {
      const row = el("div", "tool-param");
      const label = el("div", "tool-param-head");
      label.append(exact(name, "tool-param-name"), el("span", "hint", propertySummary(name, property, required.includes(name))));
      const area = growing("tool-param-description", property.description, `${tool.name}: ${name}`, (value) => {
        setDescription(property, value);
        onChange();
      });
      area.dataset.parameter = `${tool.name}.${name}`;
      area.placeholder = "No description";
      row.append(label, area);
      node.append(row);
    }
    const rest = { ...parameters };
    delete rest.properties;
    delete rest.required;
    delete rest.type;
    if (Object.keys(rest).length) node.append(el("div", "hint tool-rest", `Parameters also have ${JSON.stringify(rest)}`));
  }

  // The whole definition, exactly as it's sent. Only a custom one can be edited here.
  const whole = el("details", "tool-whole");
  whole.append(el("summary", "", "Whole definition"));
  if (tool.custom) {
    const area = el("textarea", "tool-json");
    area.spellcheck = false;
    area.setAttribute("aria-label", `${tool.name} whole definition`);
    const fill = () => {
      area.value = JSON.stringify(tool.definition, null, 2);
      grow(area);
    };
    whole.addEventListener("toggle", () => whole.open && fill());
    area.addEventListener("input", () => grow(area));
    area.addEventListener("change", () => {
      try {
        tool.definition = JSON.parse(area.value);
      } catch (err) {
        area.classList.add("bad");
        area.title = err.message;
        return;
      }
      tool.name = tool.definition.function?.name ?? tool.name;
      redraw();
      onChange();
      const again = [...document.querySelectorAll(".tool-editor")].find((n) => n.dataset.tool === tool.name);
      if (again) again.querySelector(".tool-whole").open = true;
    });
    whole.append(area);
  } else {
    const body = el("div");
    whole.addEventListener("toggle", () => whole.open && body.replaceChildren(exact(JSON.stringify(tool.definition, null, 2))));
    whole.append(body);
  }
  node.append(whole);
  return node;
}

// Sections.

// normalSection takes a section in the older form, one text, as well as the newer one.
function normalSection(section) {
  if (section.paragraphs) return section;
  const out = { name: section.name, paragraphs: (section.text ?? "").split(PARAGRAPH).map((text) => ({ text })) };
  if (section.off) out.off = true;
  return out;
}

function sectionText(section) {
  return section.paragraphs.filter((p) => !p.off).map((p) => p.text).join(PARAGRAPH);
}

function sectionSent(section) {
  return !section.off && section.paragraphs.some((p) => !p.off);
}

// allToggle is a pair of buttons that turn every item on or off.
function allToggle(what, items, redraw, onChange) {
  const row = el("div", "row all-toggle");
  for (const [label, off] of [["All on", false], ["All off", true]]) {
    const b = button(label, "mini", () => {
      for (const item of items) {
        if (off) item.off = true;
        else delete item.off;
      }
      redraw();
      onChange();
    });
    b.title = `${off ? "Turn off" : "Turn on"} every ${what}`;
    b.dataset.all = `${what}-${off ? "off" : "on"}`;
    row.append(b);
  }
  return row;
}

function sectionsEditor() {
  const node = el("div", "sections-editor");
  const draw = () => {
    start.sections = start.sections.map(normalSection);
    node.replaceChildren(allToggle("section", start.sections, draw, startChanged), ...start.sections.map((_, i) => sectionEditor(i, draw)));
    node.append(button("+ Section", "mini", () => {
      start.sections.push({ name: `section_${start.sections.length + 1}`, paragraphs: [{ text: "" }] });
      draw();
      startChanged();
      node.querySelector(".section-editor:last-of-type textarea")?.focus();
    }));
  };
  draw();
  return node;
}

// unknownVariables names the {{name}}s in text that tack won't fill.
function unknownVariables(text) {
  const unknown = [];
  scanVariables(text, start?.defaults?.variables ?? {}, (name) => name, (name) => unknown.push(name));
  return unknown;
}

function sectionEditor(i, redraw) {
  const section = start.sections[i];
  const node = el("div", `section-editor${section.off ? " is-off" : ""}`);
  node.dataset.section = section.name;
  const head = el("div", "section-head");
  const on = el("input");
  on.type = "checkbox";
  on.checked = !section.off;
  on.dataset.sectionOn = "";
  on.setAttribute("aria-label", "Send this section");
  on.addEventListener("change", () => {
    if (on.checked) delete section.off;
    else section.off = true;
    node.classList.toggle("is-off", Boolean(section.off));
    startChanged();
  });
  const name = el("input", "section-name");
  name.type = "text";
  name.value = section.name;
  name.spellcheck = false;
  name.setAttribute("aria-label", "Section name");
  name.addEventListener("input", () => {
    section.name = name.value;
    node.dataset.section = name.value;
    startChanged();
  });
  head.append(on, name, el("span", "spacer"),
    smallButton("↑", "Move up", () => moveIn(start.sections, i, -1) && (redraw(), startChanged())),
    smallButton("↓", "Move down", () => moveIn(start.sections, i, 1) && (redraw(), startChanged())),
    smallButton("✕", "Remove this section", () => {
      start.sections.splice(i, 1);
      redraw();
      startChanged();
    }));
  node.append(head);
  const paragraphs = el("div", "paragraphs");
  const drawParagraphs = (focus) => {
    paragraphs.replaceChildren(...section.paragraphs.map((paragraph, j) => {
      const row = el("div", `paragraph${paragraph.off ? " is-off" : ""}`);
      const send = el("input");
      send.type = "checkbox";
      send.checked = !paragraph.off;
      send.dataset.paragraphOn = "";
      send.setAttribute("aria-label", `Send paragraph ${j + 1} of ${section.name}`);
      send.addEventListener("change", () => {
        if (send.checked) delete paragraph.off;
        else paragraph.off = true;
        row.classList.toggle("is-off", Boolean(paragraph.off));
        startChanged();
      });
      const warn = el("div", "hint paragraph-warning");
      const check = () => {
        const unknown = unknownVariables(paragraph.text);
        warn.textContent = unknown.length ? `tack doesn't fill ${unknown.map((n) => `{{${n}}}`).join(", ")}, so it's sent as written.` : "";
        warn.hidden = !unknown.length;
      };
      const area = growing("paragraph-text", paragraph.text, `Paragraph ${j + 1} of ${section.name}`, (value) => {
        paragraph.text = value;
        check();
        startChanged();
      });
      check();
      const body = el("div", "paragraph-body");
      body.append(area, warn);
      row.append(send, body, smallButton("✕", "Remove this paragraph", () => {
        section.paragraphs.splice(j, 1);
        drawParagraphs();
        startChanged();
      }));
      return row;
    }));
    if (focus !== undefined) paragraphs.querySelectorAll("textarea")[focus]?.focus();
  };
  drawParagraphs();
  const add = button("+ Paragraph", "mini", () => {
    section.paragraphs.push({ text: "" });
    drawParagraphs(section.paragraphs.length - 1);
    startChanged();
  });
  node.append(paragraphs, add);
  return node;
}

// scanVariables calls back with each {{name}} in text, the same way tack finds them: a name
// is everything up to the next }}, and one that isn't filled is passed over from its {{.
function scanVariables(text, variables, onKnown, onUnknown) {
  let out = "";
  for (;;) {
    const open = text.indexOf("{{");
    if (open < 0) break;
    const end = text.indexOf("}}", open + 2);
    if (end < 0) break;
    const name = text.slice(open + 2, end);
    if (!Object.hasOwn(variables, name)) {
      onUnknown?.(name);
      out += text.slice(0, open + 2);
      text = text.slice(open + 2);
      continue;
    }
    out += text.slice(0, open) + onKnown(name);
    text = text.slice(end + 2);
  }
  return out + text;
}

function fill(text, variables) {
  return scanVariables(text, variables, (name) => variables[name]);
}

// The variables tack fills, with what they'd be now.
function variablesList() {
  const variables = start.defaults.variables ?? {};
  const node = el("details", "variables");
  node.append(el("summary", "", "Variables"));
  node.append(el("div", "hint", "Write {{name}} in any paragraph, and tack fills it in when the conversation starts. These are what they'd be now."));
  const table = el("div", "variable-table");
  for (const name of Object.keys(variables).sort()) {
    table.append(exact(`{{${name}}}`, "variable-name"), exact(variables[name], "variable-value"));
  }
  node.append(table);
  return node;
}

// Comparing with tack's own sections, the ones it'd start with if nothing were saved.

// paragraphDiff lines up two lists of paragraphs by their text, as kept, removed or added.
function paragraphDiff(before, after) {
  const a = before.map((p) => p.text);
  const b = after.map((p) => p.text);
  const lengths = Array.from({ length: a.length + 1 }, () => new Array(b.length + 1).fill(0));
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      lengths[i][j] = a[i] === b[j] ? lengths[i + 1][j + 1] + 1 : Math.max(lengths[i + 1][j], lengths[i][j + 1]);
    }
  }
  const out = [];
  let i = 0;
  let j = 0;
  while (i < a.length || j < b.length) {
    if (i < a.length && j < b.length && a[i] === b[j]) {
      out.push({ kind: "same", text: a[i], offBefore: Boolean(before[i].off), offAfter: Boolean(after[j].off) });
      i++;
      j++;
    } else if (j < b.length && (i === a.length || lengths[i][j + 1] >= lengths[i + 1][j])) {
      out.push({ kind: "added", text: b[j], offAfter: Boolean(after[j].off) });
      j++;
    } else {
      out.push({ kind: "removed", text: a[i], offBefore: Boolean(before[i].off) });
      i++;
    }
  }
  return out;
}

function sameSection(a, b) {
  return Boolean(a.off) === Boolean(b.off) && JSON.stringify(a.paragraphs.map((p) => [p.text, Boolean(p.off)])) === JSON.stringify(b.paragraphs.map((p) => [p.text, Boolean(p.off)]));
}

function comparison() {
  const node = el("details", "start-group comparison");
  node.append(el("summary", "", "Compared with tack's sections"));
  const body = el("div");
  node.append(body);
  node.draw = () => {
    if (!node.open) return;
    const builtin = (start.defaults.builtin?.sections ?? []).map(normalSection);
    const parts = [el("div", "hint", "tack's own sections are what a new conversation would start with if you'd saved nothing. Sections are matched by name.")];
    for (const theirs of builtin) {
      const yours = start.sections.find((s) => s.name === theirs.name);
      const box = el("div", "compare-section");
      box.dataset.compare = theirs.name;
      const head = el("div", "row");
      head.append(exact(theirs.name, "section-name-text"));
      if (!yours) {
        head.append(el("span", "tag", theirs.off ? "not in yours, off in tack's" : "not in yours"));
        const add = button("Add to yours", "mini", () => {
          start.sections.push(structuredClone(theirs));
          redrawStart();
          node.draw();
        });
        add.dataset.addSection = theirs.name;
        head.append(add);
        box.append(head);
        for (const p of theirs.paragraphs) box.append(diffParagraph({ kind: "same", text: p.text, offAfter: Boolean(p.off) }));
      } else if (sameSection(theirs, yours)) {
        head.append(el("span", "tag", "the same"));
        box.append(head);
      } else {
        head.append(el("span", "tag", "differs"));
        if (Boolean(theirs.off) !== Boolean(yours.off)) head.append(el("span", "hint", yours.off ? "off in yours, on in tack's" : "on in yours, off in tack's"));
        box.append(head);
        for (const line of paragraphDiff(theirs.paragraphs, yours.paragraphs)) box.append(diffParagraph(line));
      }
      parts.push(box);
    }
    const mine = start.sections.filter((s) => !builtin.some((b) => b.name === s.name));
    if (mine.length) parts.push(el("div", "hint", `Only in yours: ${mine.map((s) => s.name).join(", ")}.`));
    body.replaceChildren(...parts);
  };
  node.addEventListener("toggle", node.draw);
  return node;
}

function diffParagraph(line) {
  const mark = { same: " ", added: "+", removed: "−" }[line.kind];
  const row = el("div", `diff-line is-${line.kind}`);
  let note = "";
  if (line.kind === "same" && line.offBefore !== undefined) {
    if (line.offBefore !== line.offAfter) note = line.offAfter ? "off in yours" : "on in yours, off in tack's";
  } else if (line.offAfter || line.offBefore) {
    note = "off";
  }
  row.append(el("span", "diff-mark", mark), exact(line.text, "diff-text"));
  if (note) row.append(el("span", "hint", ` (${note})`));
  return row;
}

// The preview is what's sent: the text with today's date, and each definition.
function systemText() {
  const variables = start.defaults.variables ?? { date: start.defaults.date };
  return start.sections.filter(sectionSent).map((s) => fill(sectionText(s), variables)).join(PARAGRAPH);
}

function refreshPreview() {
  const box = $("start-preview-body");
  if (!box || !box.closest("details").open) return;
  const text = systemText();
  const tools = offers(start.tools).map((o) => o.definition);
  box.replaceChildren(
    el("div", "hint", text ? `System message, ${count(new TextEncoder().encode(text).length)} bytes, with variables as they'd be now. They're filled in when the conversation starts.` : "No system message."),
    exact(text || ""),
    el("div", "hint", tools.length ? `Tools, in order` : "No tools."),
    exact(tools.length ? JSON.stringify(tools, null, 2) : ""),
  );
}

let savedState = "";

function startState() {
  return JSON.stringify({ sections: start.sections, tools: start.tools });
}

function startChanged() {
  refreshPreview();
  document.querySelector(".comparison")?.draw();
  const saveButton = $("save-defaults");
  if (saveButton) saveButton.disabled = startState() === savedState;
}

async function loadBuiltins() {
  if (builtinTools) return builtinTools;
  const res = await api("/api/defaults");
  builtinTools = (await res.json()).tools ?? [];
  return builtinTools;
}

function setupStart(defaults) {
  builtinTools = defaults.tools ?? [];
  const notes = [];
  if (defaults.problem) notes.push(`The saved defaults couldn't be read, so these are tack's own: ${defaults.problem}`);
  start = {
    defaults,
    sections: structuredClone(defaults.prompt?.sections ?? []).map(normalSection),
    tools: toolList(defaults.prompt?.tools ?? null, notes),
  };
  const panel = $("start-panel");
  panel.replaceChildren();
  for (const note of notes) panel.append(el("div", "notice", note));
  const prompt = el("details", "start-group");
  prompt.open = true;
  prompt.append(el("summary", "", "System prompt"), sectionsEditor(), variablesList());
  const tools = el("details", "start-group");
  tools.open = true;
  tools.append(el("summary", "", "Tools"), el("div", "hint", "Built-in tools can only have their descriptions changed. You write the result of every call to a custom tool."));
  tools.append(toolsEditor(start.tools, startChanged));
  const compare = comparison();
  const preview = el("details", "start-group");
  preview.append(el("summary", "", "Exactly what's sent"));
  const body = el("div");
  body.id = "start-preview-body";
  preview.append(body);
  preview.addEventListener("toggle", refreshPreview);
  const settings = el("details", "start-settings start-group");
  settings.append(el("summary", "", "All settings"));
  const text = el("textarea");
  text.id = "start-settings";
  text.rows = 12;
  text.spellcheck = false;
  text.setAttribute("aria-label", "Settings for this conversation");
  text.value = JSON.stringify(defaults.settings, null, 2);
  text.addEventListener("input", refreshQuick);
  settings.append(text);
  const actions = el("div", "row start-actions");
  const save = button("Save as default", "mini primary", async () => {
    await postJSON("/api/defaults", { sections: start.sections, tools: start.tools.map((t) => ({ name: t.name, custom: t.custom || undefined, off: t.off || undefined, definition: t.definition })) });
    savedState = startState();
    startChanged();
    flash("Saved. New conversations start with this.");
  });
  save.id = "save-defaults";
  save.title = "Saves the sections and tools, but not the settings, which come from the config";
  const reset = button("Back to the default", "mini", async () => {
    const res = await api("/api/defaults");
    setupStart(await res.json());
  });
  reset.id = "reset-defaults";
  actions.append(save, reset);
  panel.append(prompt, compare, tools, preview, settings, actions);
  savedState = startState();
  startChanged();
  refreshQuick();
}

function newConversationBody() {
  const text = $("start-settings").value;
  return {
    sections: start.sections,
    tools: offers(start.tools),
    // Left alone, the config decides when the conversation starts, not when the
    // page was loaded.
    settings: text === JSON.stringify(start.defaults.settings, null, 2) ? undefined : JSON.parse(text),
  };
}

// conversationTools is the editor's list for a conversation: what it offers, in order, then
// tack's other tools, not offered.
async function conversationTools(conv) {
  const res = await api(`/api/conversations/${conv.id}/tools`);
  return toolsFrom(await res.json(), conv.custom_tools ?? []);
}

function toolsFrom(definitions, custom) {
  const out = definitions.map((d) => ({ name: d.function?.name ?? "", custom: custom.includes(d.function?.name) || undefined, definition: d }));
  for (const b of builtinTools ?? []) {
    if (!out.some((t) => !t.custom && t.name === b.name)) out.push({ name: b.name, off: true, definition: structuredClone(b.definition) });
  }
  return out;
}

// startLike opens the start panel set up the way a conversation was, to run it again.
async function startLike(conv) {
  await loadBuiltins();
  const tools = await conversationTools(conv);
  await startNew();
  const first = conv.items[0];
  start.sections = first && first.role === "system" ? [normalSection({ name: "system", text: first.text })] : [];
  start.tools.splice(0, start.tools.length, ...tools);
  $("start-settings").value = JSON.stringify(conv.settings, null, 2);
  $("start-settings").dispatchEvent(new Event("input"));
  redrawStart();
}

function redrawStart() {
  const panel = $("start-panel");
  panel.querySelector(".sections-editor").replaceWith(sectionsEditor());
  panel.querySelector(".tools-editor").replaceWith(toolsEditor(start.tools, startChanged));
  startChanged();
}

function captureStart() {
  return { sections: start.sections, tools: start.tools, settings: $("start-settings").value };
}

function restoreStart(saved) {
  if (!saved.sections || !saved.tools) return;
  start.sections = saved.sections.map(normalSection);
  start.tools.splice(0, start.tools.length, ...saved.tools);
  $("start-settings").value = saved.settings;
  $("start-settings").dispatchEvent(new Event("input"));
  redrawStart();
}

// The quick row: settings that can change at any point. In a conversation they change
// straight away. Before one starts, or in a fork, they change the settings it starts with.
function quickSettings() {
  if (current && shown.conv) return shown.conv.settings;
  const box = stagedSettingsBox();
  if (!box) return null;
  try {
    return JSON.parse(box.value);
  } catch {
    return null;
  }
}

async function setQuick(change) {
  if (current) {
    await postJSON(`/api/conversations/${current}/settings/now`, change);
    await show(current, true, true);
    return;
  }
  const box = stagedSettingsBox();
  const settings = JSON.parse(box.value);
  for (const [key, value] of Object.entries(change)) {
    if (value === "" || value === false) delete settings[key];
    else settings[key] = value;
  }
  box.value = JSON.stringify(settings, null, 2);
  box.dispatchEvent(new Event("input", { bubbles: true }));
}

let quickHosts = [];
let quickEfforts = [];

function refreshQuick() {
  const row = $("quick");
  if (!row) return;
  const settings = quickSettings();
  row.hidden = !settings;
  if (!settings) return;
  if (start?.defaults) {
    quickHosts = start.defaults.hosts ?? quickHosts;
    quickEfforts = start.defaults.efforts ?? quickEfforts;
  }
  const host = $("quick-host");
  const hosts = [...new Set([...quickHosts, settings.host].filter(Boolean))];
  const wanted = settings.host ?? "";
  if (host.dataset.options !== hosts.join("\n")) {
    host.replaceChildren(...hosts.map((h) => {
      const option = el("option", "", h);
      option.value = h;
      return option;
    }));
    host.dataset.options = hosts.join("\n");
  }
  host.value = wanted;
  const effort = $("quick-effort");
  if (effort.options.length !== quickEfforts.length + 1) {
    const unset = el("option", "", "host's default");
    unset.value = "";
    effort.replaceChildren(unset, ...quickEfforts.map((e) => {
      const option = el("option", "", e);
      option.value = e;
      return option;
    }));
  }
  effort.value = settings.reasoning_effort ?? "";
  $("quick-auto-run").checked = Boolean(settings.auto_run_code);
}

async function loadQuickChoices() {
  if (quickHosts.length) return;
  const res = await api("/api/defaults");
  const defaults = await res.json();
  quickHosts = defaults.hosts ?? [];
  quickEfforts = defaults.efforts ?? [];
  builtinTools ??= defaults.tools ?? [];
}

for (const [id, key, read] of [
  ["quick-host", "host", (n) => n.value],
  ["quick-effort", "reasoning_effort", (n) => n.value],
  ["quick-auto-run", "auto_run_code", (n) => n.checked],
]) {
  $(id).addEventListener("change", (event) => {
    setQuick({ [key]: read(event.target) }).catch((err) => {
      reportError(err);
      refreshQuick();
    });
  });
}

// A conversation's tools. A change is held and applied like settings, and in a fork that's
// being set up it waits until the fork starts. Embedded, it goes with a system message's
// editor, and saving that holds this too.
function toolsPanel(conv, embedded) {
  const node = el("details", "definitions tools-panel");
  node.append(el("summary", "", embedded ? "Tools it offers" : "Tools it offers, and their exact definitions"));
  const body = el("div");
  node.append(body);
  let tools = null;
  let before = "";
  const fill = async () => {
    await loadBuiltins();
    const staged = draftFork?.edits.get("tools");
    tools = staged ? toolsFrom(staged.offers.map((o) => o.definition), staged.offers.filter((o) => o.custom).map((o) => o.name)) : await conversationTools(conv);
    before = JSON.stringify(offers(tools));
    const parts = [toolsEditor(tools, () => {}), el("div", "hint", "Tools are part of the start of every request, so changing them means the next call misses the whole cache.")];
    if (!embedded) {
      const row = el("div", "row");
      row.append(button(draftFork ? "Use these in the fork" : "Hold change", draftFork ? "mini primary" : "mini primary net", () => node.hold()));
      parts.push(row);
    }
    body.replaceChildren(...parts);
  };
  node.changed = () => tools !== null && JSON.stringify(offers(tools)) !== before;
  node.hold = async () => {
    if (!tools) return;
    if (draftFork) {
      draftFork.edits.set("tools", { offers: offers(tools) });
      before = JSON.stringify(offers(tools));
      flash("The fork will start with these tools.");
      return;
    }
    await postConversation(conv.id, "tools", { tools: offers(tools) });
    await show(conv.id);
  };
  node.addEventListener("toggle", () => {
    if (node.open && tools === null) fill().catch(reportError);
  });
  if (embedded) {
    node.open = true;
    fill().catch(reportError);
  }
  return node;
}
