// Markdown's tree (markdown.js) drawn as DOM, with text set as text, never as HTML, and the
// small things that go with it: hidden characters shown, Python highlighted (highlight.js),
// and copying. Both the conversation page and the prompt page use it, as plain functions.
"use strict";

function mdEl(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

// copyText works over plain HTTP too, where navigator.clipboard doesn't exist.
async function copyText(text) {
  if (navigator.clipboard) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      // Fall through.
    }
  }
  const area = mdEl("textarea");
  area.value = text;
  area.setAttribute("readonly", "");
  area.style.position = "fixed";
  area.style.opacity = "0";
  document.body.append(area);
  area.select();
  document.execCommand("copy");
  area.remove();
}

// A code block's Copy button.
function mdCopyButton(label, getText) {
  const b = mdEl("button", "mini copy-button", label);
  b.type = "button";
  b.addEventListener("click", async () => {
    await copyText(getText());
    b.textContent = "Copied";
    setTimeout(() => {
      b.textContent = label;
    }, 1200);
  });
  return b;
}

// Characters that could hide what a call really does from whoever is approving it.
const HIDDEN = /(?![\t\n])[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/gu;

function appendExact(node, text) {
  let at = 0;
  for (const match of text.matchAll(HIDDEN)) {
    node.append(text.slice(at, match.index));
    const code = match[0].codePointAt(0).toString(16).toUpperCase().padStart(4, "0");
    node.append(mdEl("span", "ctrl", `U+${code}`));
    at = match.index + match[0].length;
  }
  node.append(text.slice(at));
}

function appendPython(node, text) {
  for (const token of tackHighlight.python(text)) {
    if (token.type === "plain") appendExact(node, token.text);
    else {
      const span = mdEl("span", `p-${token.type}`);
      appendExact(span, token.text);
      node.append(span);
    }
  }
}

// Markdown tree to DOM.

function renderInline(nodes, parent) {
  for (const n of nodes) {
    switch (n.type) {
      case "text": parent.append(n.text); break;
      case "break": parent.append(mdEl("br")); break;
      case "code": parent.append(mdEl("code", "", n.text)); break;
      case "strong": case "em": case "del": {
        const tag = { strong: "strong", em: "em", del: "del" }[n.type];
        const node = mdEl(tag);
        renderInline(n.children, node);
        parent.append(node);
        break;
      }
      case "link": renderLink(n, parent); break;
    }
  }
}

function plainText(nodes) {
  return nodes.map((n) => n.text || (n.children ? plainText(n.children) : "")).join("");
}

function renderLink(n, parent) {
  const a = mdEl("a");
  a.href = n.url;
  a.target = "_blank";
  a.rel = "noopener noreferrer nofollow";
  a.title = n.url;
  if (n.image) a.append("Image: ");
  renderInline(n.children, a);
  parent.append(a);
  // A link's text can say anything, so show where it really goes.
  let host = "";
  try { host = new URL(n.url).hostname; } catch { /* Shown without a host. */ }
  if (host && !plainText(n.children).includes(host)) {
    parent.append(mdEl("span", "link-host", `(${host})`));
  }
}

function renderBlocks(blocks, parent) {
  for (const b of blocks) {
    switch (b.type) {
      case "paragraph": {
        const p = mdEl("p");
        renderInline(b.children, p);
        parent.append(p);
        break;
      }
      case "heading": {
        const h = mdEl(`h${b.level}`);
        renderInline(b.children, h);
        parent.append(h);
        break;
      }
      case "rule": parent.append(mdEl("hr")); break;
      case "quote": {
        const q = mdEl("blockquote");
        renderBlocks(b.children, q);
        parent.append(q);
        break;
      }
      case "code": parent.append(renderCode(b)); break;
      case "list": parent.append(renderList(b)); break;
      case "table": parent.append(renderTable(b)); break;
    }
  }
}

function renderCode(b) {
  const box = mdEl("div", "codeblock");
  const head = mdEl("div", "codeblock-head");
  head.append(mdEl("span", "", b.lang || "text"), mdCopyButton("Copy", () => b.text));
  const pre = mdEl("pre");
  const code = mdEl("code");
  if (b.lang === "python" || b.lang === "py") appendPython(code, b.text);
  else code.textContent = b.text;
  pre.append(code);
  box.append(head, pre);
  return box;
}

function renderList(b) {
  const list = mdEl(b.ordered ? "ol" : "ul");
  if (b.ordered && b.start !== 1) list.start = b.start;
  for (const item of b.items) {
    const li = mdEl("li");
    if (item.task !== null) {
      li.className = "task";
      const box = mdEl("input");
      box.type = "checkbox";
      box.checked = item.task;
      box.disabled = true;
      li.append(box);
    }
    const blocks = item.blocks;
    if (!b.loose && blocks[0] && blocks[0].type === "paragraph") {
      renderInline(blocks[0].children, li);
      renderBlocks(blocks.slice(1), li);
    } else {
      renderBlocks(blocks, li);
    }
    list.append(li);
  }
  return list;
}

function renderTable(b) {
  const wrap = mdEl("div", "tablewrap");
  const table = mdEl("table");
  const cell = (tag, nodes, i) => {
    const c = mdEl(tag, b.align[i] ? `align-${b.align[i]}` : "");
    renderInline(nodes, c);
    return c;
  };
  const head = mdEl("thead");
  const headRow = mdEl("tr");
  b.head.forEach((nodes, i) => headRow.append(cell("th", nodes, i)));
  head.append(headRow);
  const body = mdEl("tbody");
  for (const row of b.rows) {
    const tr = mdEl("tr");
    row.forEach((nodes, i) => tr.append(cell("td", nodes, i)));
    body.append(tr);
  }
  table.append(head, body);
  wrap.append(table);
  return wrap;
}

function setMarkdown(node, text) {
  const fragment = document.createDocumentFragment();
  renderBlocks(tackMarkdown.parse(text), fragment);
  node.replaceChildren(fragment);
}
