// The codebase and documentation pages. Both show tack's source exactly as the running
// binary was built from it, and take comments on it: on everything, a file, some lines, or
// a stretch of text. Comments are kept by tack and read later as a batch. Everything from
// the server reaches the page as text, never markup.
"use strict";

const $ = (id) => document.getElementById(id);
const pageRequired = document.querySelector('meta[name="tack-page"]').dataset.required;
const PAGE = document.body.dataset.page; // "code" or "docs"

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function button(label, className, onClick) {
  const b = el("button", className, label);
  b.type = "button";
  b.addEventListener("click", async () => {
    if (b.disabled) return;
    b.disabled = true;
    try {
      await onClick(b);
    } catch (err) {
      showError(err);
    } finally {
      b.disabled = false;
    }
  });
  return b;
}

async function api(path, options) {
  let res;
  try {
    res = await fetch(path, { ...options, headers: { ...options?.headers, "X-Tack-Page-Required": pageRequired } });
  } catch {
    throw new Error("Couldn't reach tack, so that may not have happened.");
  }
  if (!res.ok) throw new Error((await res.text()) || `HTTP ${res.status}`);
  return res;
}

async function getJSON(path) {
  return (await api(path)).json();
}

function postJSON(path, body) {
  return api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
}

function showError(err) {
  const box = $("error");
  box.textContent = err.message || String(err);
  box.hidden = false;
  clearTimeout(showError.timer);
  showError.timer = setTimeout(() => (box.hidden = true), 8000);
}

const codePoints = (text) => [...text].length;
const short = (revision) => (revision || "unknown").slice(0, 7);
const when = (time) => new Date(time).toLocaleString();

// State shared by both pages.
const state = {
  revision: "",
  feedback: [],
  // What's being commented on, if anything: { path, startLine, endLine, startColumn, endColumn, quote }.
  target: null,
  // Lines picked by their numbers, for a comment on them.
  picked: null,
  // The file shown, as { path, text }, if it's one that can be commented on.
  file: null,
};

async function loadFeedback() {
  const got = await getJSON("/api/feedback");
  state.feedback = got.feedback;
  drawComments();
}

// Describing where a comment is.

function targetLabel(f) {
  if (!f.path) return "The whole of tack";
  if (!f.start_line) return f.path;
  const lines = f.start_line === f.end_line ? `line ${f.start_line}` : `lines ${f.start_line}–${f.end_line}`;
  if (f.start_column === undefined || f.start_column === null) return `${f.path}, ${lines}`;
  return `${f.path}, ${lines}, a selection`;
}

function toWire(target, text) {
  const out = { revision: state.revision, text };
  if (target.path) out.path = target.path;
  if (target.startLine) {
    out.start_line = target.startLine;
    out.end_line = target.endLine;
  }
  if (target.startColumn !== undefined) {
    out.start_column = target.startColumn;
    out.end_column = target.endColumn;
  }
  if (target.quote) out.quote = target.quote;
  return out;
}

// The comment form. One at a time.

function openForm(target) {
  state.target = target;
  const form = $("comment-form");
  form.hidden = false;
  $("comment-target").textContent = targetLabel(toWire(target, ""));
  const quote = $("comment-quote");
  quote.textContent = target.quote ?? "";
  quote.hidden = !target.quote;
  $("comment-text").value = "";
  $("comment-text").focus();
}

function closeForm() {
  state.target = null;
  $("comment-form").hidden = true;
  clearPicked();
}

async function saveComment() {
  const text = $("comment-text").value;
  if (!text.trim()) throw new Error("The comment is empty.");
  await postJSON("/api/feedback", toWire(state.target, text));
  closeForm();
  window.getSelection()?.removeAllRanges();
  await loadFeedback();
  flash("Saved. It'll be read with the rest when you ask.");
}

function flash(text) {
  const box = $("flash");
  box.textContent = text;
  box.hidden = false;
  clearTimeout(flash.timer);
  flash.timer = setTimeout(() => (box.hidden = true), 3000);
}

// The comments panel lists what's been written about what's shown, or everything.

function commentNode(f) {
  const node = el("div", `comment${f.read ? " is-read" : ""}`);
  node.dataset.comment = f.id;
  const head = el("div", "comment-head");
  const where = el("span", "comment-where", targetLabel(f));
  head.append(where);
  if (f.revision !== state.revision) head.append(el("span", "tag", `on ${short(f.revision)}, an older version`));
  head.append(el("span", "tag", f.read ? `read ${when(f.read)}` : "not read yet"));
  head.append(el("span", "spacer"));
  if (f.path && (f.revision === state.revision || !f.start_line)) {
    const go = button("Show", "mini", () => showTarget(f));
    head.append(go);
  }
  if (!f.read) {
    head.append(button("Remove", "mini", async () => {
      await postJSON(`/api/feedback/${f.id}/remove`, {});
      await loadFeedback();
    }));
  }
  node.append(head);
  if (f.quote) node.append(el("pre", "comment-quote", f.quote));
  node.append(el("div", "comment-text", f.text));
  node.append(el("div", "hint", `Written ${when(f.time)}`));
  return node;
}

function relevant(f) {
  const scope = $("comments-scope")?.value ?? "here";
  if (scope === "all") return true;
  if (scope === "unread") return !f.read;
  if (!state.file) return !f.path;
  return f.path === state.file.path;
}

function drawComments() {
  const list = $("comments");
  if (!list) return;
  const shown = state.feedback.filter(relevant);
  list.replaceChildren(...(shown.length ? shown.slice().reverse().map(commentNode) : [el("div", "hint", "No comments here yet.")]));
  const unread = state.feedback.filter((f) => !f.read).length;
  $("comments-count").textContent = unread ? `${unread} not read yet` : "";
  markCommentedLines();
}

// The lines view: a file's text, one row per line, with numbers to pick lines by.

function lineView(file) {
  const view = el("div", "lines");
  view.dataset.path = file.path;
  const lines = file.text.split("\n");
  // A file that ends with a newline doesn't have an empty last line.
  if (lines.length > 1 && lines.at(-1) === "") lines.pop();
  lines.forEach((text, i) => {
    const row = el("div", "line");
    row.dataset.line = String(i + 1);
    const number = el("button", "line-number", String(i + 1));
    number.type = "button";
    number.title = "Pick this line. Shift-click to pick through to it.";
    number.addEventListener("click", (event) => pickLine(i + 1, event.shiftKey));
    const body = el("span", "line-text", text);
    body.dataset.line = String(i + 1);
    row.append(number, body);
    view.append(row);
  });
  view.addEventListener("mouseup", () => setTimeout(offerSelection, 0));
  return view;
}

function fileLines() {
  const lines = state.file.text.split("\n");
  if (lines.length > 1 && lines.at(-1) === "") lines.pop();
  return lines;
}

function pickLine(n, extend) {
  if (extend && state.picked) {
    state.picked = { start: Math.min(state.picked.anchor, n), end: Math.max(state.picked.anchor, n), anchor: state.picked.anchor };
  } else {
    state.picked = { start: n, end: n, anchor: n };
  }
  history.replaceState(null, "", `${location.pathname}${location.hash.split(":L")[0]}:L${state.picked.start}${state.picked.end !== state.picked.start ? `-${state.picked.end}` : ""}`);
  drawPicked();
}

function clearPicked() {
  state.picked = null;
  drawPicked();
}

function drawPicked() {
  for (const row of document.querySelectorAll(".line.is-picked")) row.classList.remove("is-picked");
  const bar = $("pick-bar");
  if (!state.picked) {
    bar.hidden = true;
    return;
  }
  for (let n = state.picked.start; n <= state.picked.end; n++) document.querySelector(`.line[data-line="${n}"]`)?.classList.add("is-picked");
  const { start, end } = state.picked;
  $("pick-label").textContent = start === end ? `Line ${start}` : `Lines ${start}–${end}`;
  bar.hidden = false;
}

function commentOnPicked() {
  const { start, end } = state.picked;
  const quote = fileLines().slice(start - 1, end).join("\n");
  openForm({ path: state.file.path, startLine: start, endLine: end, quote });
}

// A stretch of text that's been highlighted can be commented on, where it is exactly.
function selectionTarget() {
  const selection = window.getSelection();
  if (!selection || selection.isCollapsed || !state.file) return null;
  const range = selection.getRangeAt(0);
  const point = (node, offset) => {
    const line = (node.nodeType === Node.TEXT_NODE ? node.parentElement : node)?.closest?.(".line-text");
    if (!line) return null;
    // A line's text is one text node, so an offset into it is an offset into the line.
    const text = line.textContent;
    const units = node.nodeType === Node.TEXT_NODE ? offset : offset === 0 ? 0 : text.length;
    return { line: Number(line.dataset.line), column: codePoints(text.slice(0, units)) };
  };
  const start = point(range.startContainer, range.startOffset);
  const end = point(range.endContainer, range.endOffset);
  if (!start || !end) return null;
  if (start.line === end.line && start.column === end.column) return null;
  const lines = fileLines();
  const pieces = [];
  for (let n = start.line; n <= end.line; n++) {
    const chars = [...lines[n - 1]];
    const from = n === start.line ? start.column : 0;
    const to = n === end.line ? end.column : chars.length;
    pieces.push(chars.slice(from, to).join(""));
  }
  return { path: state.file.path, startLine: start.line, endLine: end.line, startColumn: start.column, endColumn: end.column, quote: pieces.join("\n") };
}

function offerSelection() {
  const target = selectionTarget();
  const bar = $("selection-bar");
  bar.hidden = !target;
  if (target) {
    bar.target = target;
    const lines = target.startLine === target.endLine ? `line ${target.startLine}` : `lines ${target.startLine}–${target.endLine}`;
    $("selection-label").textContent = `${codePoints(target.quote)} characters on ${lines}`;
  }
}

function markCommentedLines() {
  for (const row of document.querySelectorAll(".line.has-comment")) row.classList.remove("has-comment");
  if (!state.file) return;
  for (const f of state.feedback) {
    if (f.path !== state.file.path || !f.start_line || f.revision !== state.revision) continue;
    for (let n = f.start_line; n <= f.end_line; n++) document.querySelector(`.line[data-line="${n}"]`)?.classList.add("has-comment");
  }
}

// showTarget opens what a comment is about.
async function showTarget(f) {
  if (PAGE === "code") {
    if (f.path.startsWith(`${codeState.docsDir}/`)) {
      location.href = `/docs#tack/${f.path.slice(codeState.docsDir.length + 1)}`;
      return;
    }
    location.hash = `#${f.path}${f.start_line ? `:L${f.start_line}${f.end_line !== f.start_line ? `-${f.end_line}` : ""}` : ""}`;
  } else {
    const page = docsState.pages.find((p) => p.tree === "tack" && `${docsState.sourceDir}/${p.path}` === f.path);
    if (page) location.hash = `#tack/${page.path}${f.start_line ? `:L${f.start_line}${f.end_line !== f.start_line ? `-${f.end_line}` : ""}` : ""}`;
  }
}

// Picks lines named in the address, like #path:L10-12, after a file is drawn.
function pickFromHash(hash) {
  const m = /:L(\d+)(?:-(\d+))?$/.exec(hash);
  if (!m) return;
  const start = Number(m[1]);
  const end = Number(m[2] ?? m[1]);
  state.picked = { start, end, anchor: start };
  drawPicked();
  document.querySelector(`.line[data-line="${start}"]`)?.scrollIntoView({ block: "center" });
}

function setupCommon() {
  $("comment-save").addEventListener("click", () => saveComment().catch(showError));
  $("comment-cancel").addEventListener("click", closeForm);
  $("comment-text").addEventListener("keydown", (event) => {
    if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) saveComment().catch(showError);
    if (event.key === "Escape") closeForm();
  });
  $("pick-comment").addEventListener("click", commentOnPicked);
  $("pick-clear").addEventListener("click", () => {
    clearPicked();
    history.replaceState(null, "", `${location.pathname}${location.hash.split(":L")[0]}`);
  });
  $("selection-comment").addEventListener("click", () => {
    const target = $("selection-bar").target;
    $("selection-bar").hidden = true;
    if (target) openForm(target);
  });
  $("comments-scope").addEventListener("change", drawComments);
  document.addEventListener("selectionchange", () => {
    if (window.getSelection()?.isCollapsed) $("selection-bar").hidden = true;
  });
}

// A tree of paths as nested lists, with directories that open and close.
function pathTree(paths, href, isCurrent) {
  const root = { dirs: new Map(), files: [] };
  for (const p of paths) {
    const parts = p.split("/");
    let node = root;
    for (const dir of parts.slice(0, -1)) {
      if (!node.dirs.has(dir)) node.dirs.set(dir, { dirs: new Map(), files: [] });
      node = node.dirs.get(dir);
    }
    node.files.push({ name: parts.at(-1), path: p });
  }
  const draw = (node, prefix) => {
    const list = el("ul", "tree");
    for (const [name, child] of [...node.dirs].sort(([a], [b]) => a.localeCompare(b))) {
      const item = el("li");
      const details = el("details");
      const full = prefix + name + "/";
      details.open = isCurrent(full, true);
      details.append(el("summary", "", name), draw(child, full));
      item.append(details);
      list.append(item);
    }
    for (const file of node.files.sort((a, b) => a.name.localeCompare(b.name))) {
      const item = el("li");
      const a = el("a", "", file.name);
      a.href = href(file.path);
      if (isCurrent(file.path, false)) a.setAttribute("aria-current", "page");
      item.append(a);
      list.append(item);
    }
    return list;
  };
  return draw(root, "");
}

// The codebase page.

const codeState = { files: [], docsDir: "docs/model" };

async function setupCode() {
  const got = await getJSON("/api/source");
  state.revision = got.revision;
  codeState.docsDir = got.docs_dir;
  codeState.files = got.files.filter((f) => !f.path.startsWith(`${got.docs_dir}/`));
  $("revision").textContent = `Revision ${short(got.revision)}, as it's running now`;
  window.addEventListener("hashchange", () => showCode().catch(showError));
  await loadFeedback();
  await showCode();
}

function drawCodeTree(current) {
  const paths = codeState.files.map((f) => f.path);
  const tree = pathTree(paths, (p) => `#${p}`, (p, dir) => (dir ? current.startsWith(p) : current === p));
  const docs = el("a", "tree-docs", `${codeState.docsDir}/ is on the documentation page`);
  docs.href = "/docs";
  $("tree").replaceChildren(tree, docs);
}

async function showCode() {
  const hash = decodeURIComponent(location.hash.slice(1));
  const path = hash.split(":L")[0];
  closeForm();
  drawCodeTree(path);
  const main = $("view");
  if (!path) {
    state.file = null;
    $("view-title").textContent = "tack";
    const actions = $("view-actions");
    actions.replaceChildren(button("Comment on the whole of tack", "mini primary", () => openForm({})));
    main.replaceChildren(el("p", "hint", `Pick a file on the left. Comments are kept and read together later, when you ask for them to be. Everything here is revision ${short(state.revision)}, the one running now.`));
    drawComments();
    return;
  }
  if (path.startsWith(`${codeState.docsDir}/`)) {
    location.replace(`/docs#tack/${path.slice(codeState.docsDir.length + 1)}`);
    return;
  }
  const file = await getJSON(`/api/source/file?path=${encodeURIComponent(path)}`);
  if (file.revision !== state.revision) {
    showError("tack has been deployed since this page loaded. Reload it to see what's running now.");
  }
  $("view-title").textContent = path;
  $("view-actions").replaceChildren(button("Comment on this file", "mini primary", () => openForm({ path })));
  if (file.binary) {
    main.replaceChildren(el("p", "hint", `This isn't text: ${file.size.toLocaleString()} bytes.`));
    state.file = { path, text: "" };
    drawComments();
    return;
  }
  state.file = { path, text: file.text };
  main.replaceChildren(lineView(state.file));
  drawComments();
  pickFromHash(hash);
}

// The documentation page.

const docsState = { pages: [], sourceDir: "docs/model", page: null, mode: "rendered", editing: false };

async function setupDocs() {
  const got = await getJSON("/api/docs");
  state.revision = got.revision;
  docsState.pages = got.pages;
  docsState.sourceDir = got.source_dir;
  $("revision").textContent = `tack's pages are revision ${short(got.revision)}, as it's running now`;
  try {
    docsState.mode = localStorage.getItem("tack-docs-mode") || "rendered";
  } catch {
    // It's only a convenience.
  }
  for (const b of document.querySelectorAll("[data-mode]")) {
    b.addEventListener("click", () => {
      docsState.mode = b.dataset.mode;
      try {
        localStorage.setItem("tack-docs-mode", docsState.mode);
      } catch {
        // It's only a convenience.
      }
      showDoc().catch(showError);
    });
  }
  $("new-page").addEventListener("click", () => newPage());
  window.addEventListener("hashchange", () => showDoc().catch(showError));
  await loadFeedback();
  await showDoc();
}

async function reloadPages() {
  docsState.pages = (await getJSON("/api/docs")).pages;
}

function drawDocsTree(current) {
  const parts = [];
  for (const [tree, title] of [["tack", "tack's"], ["user", "Yours"]]) {
    parts.push(el("h2", "side-head", title));
    const paths = docsState.pages.filter((p) => p.tree === tree).map((p) => p.path);
    if (!paths.length) parts.push(el("p", "hint tree-empty", tree === "user" ? "None yet." : "None."));
    else parts.push(pathTree(paths, (p) => `#${tree}/${p}`, (p, dir) => (dir ? current.startsWith(`${tree}/${p}`) : current === `${tree}/${p}`)));
  }
  $("tree").replaceChildren(...parts);
}

function sourcePath(page) {
  return page.tree === "tack" ? `${docsState.sourceDir}/${page.path}` : null;
}

async function showDoc() {
  const hash = decodeURIComponent(location.hash.slice(1));
  const where = hash.split(":L")[0];
  closeForm();
  docsState.editing = false;
  drawDocsTree(where);
  const slash = where.indexOf("/");
  const tree = slash > 0 ? where.slice(0, slash) : "";
  const path = slash > 0 ? where.slice(slash + 1) : "";
  const main = $("view");
  $("modes").hidden = !path;
  if (!path) {
    state.file = null;
    docsState.page = null;
    $("view-title").textContent = "Documentation";
    $("view-actions").replaceChildren();
    main.replaceChildren(
      el("p", "", "Code the model runs can read these under /docs: tack's in /docs/tack, and yours in /docs/user, with /docs/index.md listing them all."),
      el("p", "hint", "tack's pages are part of its code, so they change when it does, and you can comment on them like the rest of the code. Yours are files in the docs directory next to tack's config, and you can add, change and remove them here. Pages link to each other with [[Page]] or [[Page|label]], where a page's name is its path without .md. The model reads those as relative Markdown links, and {{name}} variables filled in, as For the model shows."),
    );
    drawComments();
    return;
  }
  const page = await getJSON(`/api/docs/page?tree=${encodeURIComponent(tree)}&path=${encodeURIComponent(path)}`);
  docsState.page = page;
  if (page.revision !== state.revision) showError("tack has been deployed since this page loaded. Reload it to see what's running now.");
  $("view-title").textContent = `${tree === "tack" ? "tack's" : "Yours"}: ${path}`;
  for (const b of document.querySelectorAll("[data-mode]")) b.setAttribute("aria-pressed", String(b.dataset.mode === docsState.mode));
  const actions = [];
  const source = sourcePath(page);
  state.file = source ? { path: source, text: page.text } : null;
  if (source) actions.push(button("Comment on this page", "mini primary", () => openForm({ path: source })));
  if (tree === "user") {
    actions.push(button("Edit", "mini", () => editPage(page)));
    actions.push(button("Remove", "mini", async () => {
      if (!confirm(`Remove ${path}? It's deleted from the docs directory.`)) return;
      await postJSON("/api/docs/remove", { path });
      await reloadPages();
      location.hash = "";
    }));
  }
  $("view-actions").replaceChildren(...actions);
  if (docsState.mode === "raw") {
    main.replaceChildren(source ? lineView(state.file) : el("pre", "raw-page", page.text));
    if (!source) main.prepend(el("p", "hint", "Comments are for tack's own pages. This one is yours to change."));
  } else if (docsState.mode === "model") {
    main.replaceChildren(el("p", "hint", "What code the model runs reads at " + `/docs/${tree}/${path}`), el("pre", "raw-page", page.for_model));
  } else {
    main.replaceChildren(renderedPage(page));
  }
  drawComments();
  pickFromHash(hash);
}

function newPage() {
  editPage({ tree: "user", path: "", text: "" });
}

function editPage(page) {
  docsState.editing = true;
  closeForm();
  state.file = null;
  $("modes").hidden = true;
  $("view-title").textContent = page.path ? `Editing yours: ${page.path}` : "A new page of yours";
  const name = el("input", "page-path");
  name.type = "text";
  name.value = page.path || "new-page.md";
  name.spellcheck = false;
  name.setAttribute("aria-label", "Path, ending in .md");
  name.id = "page-path";
  const text = el("textarea", "page-text");
  text.value = page.text;
  text.spellcheck = true;
  text.id = "page-text";
  text.setAttribute("aria-label", "Page text, in Markdown");
  const save = button("Save", "mini primary", async () => {
    await postJSON("/api/docs/save", { path: name.value, text: text.value, from: page.path || "" });
    await reloadPages();
    docsState.editing = false;
    if (location.hash === `#user/${name.value}`) await showDoc();
    else location.hash = `#user/${name.value}`;
  });
  save.id = "page-save";
  const cancel = button("Cancel", "mini", () => showDoc());
  $("view-actions").replaceChildren(save, cancel);
  const label = el("label", "field-label", "Path, under the docs directory");
  $("view").replaceChildren(label, name, text, el("p", "hint", "Markdown. Link to other pages with [[Page]] or [[Page|label]]. {{name}} variables are filled in for the model."));
  text.focus();
}

// renderedPage shows a page's Markdown, with variables filled in as they'd be now, and its
// [[links]] going to the pages they name.
// Links are swapped for markers the parser keeps as text, then drawn as links here.
function renderedPage(page) {
  const box = el("div", "prose doc-prose");
  const targets = [];
  let i = 0;
  const marked = page.filled.replace(/\[\[([^\[\]\n]+?)\]\]/g, (whole, inside) => {
    const [target, label] = inside.split("|");
    const to = page.links.find((l) => l.target === target)?.to ?? null;
    targets.push({ label: label ?? target, to, whole });
    return `${i++}`;
  });
  renderBlocks(tackMarkdown.parse(marked), box, targets);
  return box;
}

function renderText(text, parent, targets) {
  const parts = text.split(/(\d+)/);
  parts.forEach((part, j) => {
    if (j % 2 === 0) {
      if (part) parent.append(part);
      return;
    }
    const t = targets[Number(part)];
    if (!t) return;
    if (!t.to) {
      parent.append(el("span", "broken-link", t.whole));
      return;
    }
    const a = el("a", "", t.label);
    a.href = `#${t.to.tree}/${t.to.path}`;
    parent.append(a);
  });
}

function renderInline(nodes, parent, targets) {
  for (const n of nodes) {
    switch (n.type) {
      case "text": renderText(n.text, parent, targets); break;
      // Documentation is written wrapped, so a line break inside a paragraph is a space.
      case "break": parent.append(" "); break;
      case "code": parent.append(el("code", "", n.text.replace(/(\d+)/g, (_, k) => targets[Number(k)]?.whole ?? ""))); break;
      case "strong": case "em": case "del": {
        const node = el({ strong: "strong", em: "em", del: "del" }[n.type]);
        renderInline(n.children, node, targets);
        parent.append(node);
        break;
      }
      case "link": {
        const a = el("a");
        a.href = n.url;
        a.target = "_blank";
        a.rel = "noopener noreferrer nofollow";
        a.title = n.url;
        renderInline(n.children, a, targets);
        parent.append(a);
        break;
      }
    }
  }
}

function unmark(text, targets) {
  return text.replace(/(\d+)/g, (_, k) => targets[Number(k)]?.whole ?? "");
}

function renderBlocks(blocks, parent, targets) {
  for (const b of blocks) {
    switch (b.type) {
      case "paragraph": {
        const p = el("p");
        renderInline(b.children, p, targets);
        parent.append(p);
        break;
      }
      case "heading": {
        const h = el(`h${b.level}`);
        renderInline(b.children, h, targets);
        parent.append(h);
        break;
      }
      case "rule": parent.append(el("hr")); break;
      case "quote": {
        const q = el("blockquote");
        renderBlocks(b.children, q, targets);
        parent.append(q);
        break;
      }
      case "code": {
        const pre = el("pre");
        const code = el("code");
        const text = unmark(b.text, targets);
        if (b.lang === "python" || b.lang === "py") {
          for (const token of tackHighlight.python(text)) code.append(token.type === "plain" ? token.text : el("span", `p-${token.type}`, token.text));
        } else {
          code.textContent = text;
        }
        pre.append(code);
        parent.append(pre);
        break;
      }
      case "list": {
        const list = el(b.ordered ? "ol" : "ul");
        if (b.ordered && b.start !== 1) list.start = b.start;
        for (const item of b.items) {
          const li = el("li");
          const blocks2 = item.blocks;
          if (!b.loose && blocks2[0] && blocks2[0].type === "paragraph") {
            renderInline(blocks2[0].children, li, targets);
            renderBlocks(blocks2.slice(1), li, targets);
          } else {
            renderBlocks(blocks2, li, targets);
          }
          list.append(li);
        }
        parent.append(list);
        break;
      }
      case "table": {
        const wrap = el("div", "tablewrap");
        const table = el("table");
        const row = (cells, tag) => {
          const tr = el("tr");
          cells.forEach((nodes, k) => {
            const cell = el(tag, b.align[k] ? `align-${b.align[k]}` : "");
            renderInline(nodes, cell, targets);
            tr.append(cell);
          });
          return tr;
        };
        const head = el("thead");
        head.append(row(b.head, "th"));
        const body = el("tbody");
        for (const r of b.rows) body.append(row(r, "td"));
        table.append(head, body);
        wrap.append(table);
        parent.append(wrap);
        break;
      }
    }
  }
}

setupCommon();
(PAGE === "code" ? setupCode() : setupDocs()).catch(showError);
