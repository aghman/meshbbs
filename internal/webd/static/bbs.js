// The Screen renderer (webui.md §2, §4).
//
// The server sends a whole Screen on every change and this turns it into DOM.
// There is no diffing: screens are small, and a full re-render has no
// reconciliation bug class to get wrong.
//
// Two rules hold the design together:
//
//   1. Everything is a keystroke. Clicking the [M] row sends "m"; tapping the
//      `enter open` button sends "enter". The server never learns it was a
//      click, so there is no browser-only path through the BBS.
//
//   2. Geometry lives HERE, not in the model. The server sends whole strings
//      and column hints; wrapping, scrolling and eliding are this file's job.
//      That is why a long area description reads in full here and gets cut at
//      column 26 over SSH.

let socket = null;

const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  // textContent, never innerHTML. Every string here is user-controlled
  // somewhere — nicks, subjects, chat lines, area descriptions — and the
  // terminal sanitiser upstream neutralises escape sequences, not markup.
  if (text !== undefined) n.textContent = text;
  return n;
};

const LEVELS = ["body", "muted", "heading", "accent", "error", "success"];
const levelClass = (n) => LEVELS[n] || "body";

const send = (msg) => {
  if (socket && socket.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify(msg));
  }
};

const key = (name) => send({ key: name });

// --- blocks ----------------------------------------------------------------

function renderText(b) {
  const p = el("div", "block text" + (b.wrap ? " wrap" : ""));
  for (const line of b.lines || []) {
    const row = el("div", "line");
    if (!line || line.length === 0) {
      row.appendChild(el("span", "", " "));
    } else {
      for (const span of line) {
        row.appendChild(el("span", levelClass(span.level), span.text));
      }
    }
    p.appendChild(row);
  }
  return p;
}

function renderChoices(b) {
  const list = el("div", "block choices");
  for (const item of b.items || []) {
    const row = el("button", "choice");
    row.appendChild(el("span", "hotkey", "[" + item.key + "]"));
    row.appendChild(el("span", "choice-label", item.label));
    // The whole row is the target, which is what makes this usable on a phone
    // where a single bracketed letter is not.
    row.addEventListener("click", () => key(item.key));
    list.appendChild(row);
  }
  return list;
}

function renderTable(b) {
  const wrap = el("div", "block table-wrap");
  if (b.title) wrap.appendChild(el("div", "section-title", b.title));

  if (!b.rows || b.rows.length === 0) {
    if (b.empty) wrap.appendChild(el("div", "muted", b.empty));
    return wrap;
  }

  const table = el("table", "table");
  if (b.header && b.header.length) {
    const tr = el("tr");
    for (const h of b.header) tr.appendChild(el("th", "", h));
    table.appendChild(el("thead")).appendChild(tr);
  }

  const body = el("tbody");
  b.rows.forEach((row, i) => {
    const tr = el("tr", i === b.selected ? "row selected" : "row");
    (row.cells || []).forEach((cell, c) => {
      const td = el("td", "", cell);
      // Carry the column name onto every cell so the narrow layout can label
      // it. A table reflowed to cards without labels is a stack of bare values
      // — fine for an area list, useless for the sysop panel where "yes yes no"
      // means nothing without its heading.
      if (b.header && b.header[c]) td.dataset.label = b.header[c];
      tr.appendChild(td);
    });
    if (b.selected >= 0) {
      tr.tabIndex = 0;
      // Select then open, so one tap does what a click should. The cursor move
      // is a real model update, so the server and the page never disagree
      // about which row is current.
      tr.addEventListener("click", () => {
        send({ select: i });
        key("enter");
      });
      tr.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
          send({ select: i });
          key("enter");
        }
      });
    }
    body.appendChild(tr);
  });
  table.appendChild(body);
  wrap.appendChild(table);
  return wrap;
}

function renderArticle(b) {
  const art = el("article", "block article");
  art.appendChild(el("h2", "article-heading", b.heading));
  art.appendChild(el("div", "muted article-meta", b.meta));
  // Bodies keep their own line breaks; CSS re-wraps the long lines.
  art.appendChild(el("div", "article-body", b.body));
  return art;
}

function renderForm(b) {
  const form = el("div", "block form");
  for (const f of b.fields || []) {
    const row = el("div", "field-row");
    // The terminal's textarea has no prompt, so body fields arrive unlabelled.
    // A browser wants a label regardless — presentation is the renderer's job.
    const label = (f.label || f.name || "").replace(/:\s*$/, "");
    if (label) row.appendChild(el("label", "field-label", label));

    if (f.done) {
      row.appendChild(el("div", "muted", "•".repeat(f.value.length)));
      form.appendChild(row);
      continue;
    }

    const input = f.multiline ? el("textarea", "input") : el("input", "input");
    input.value = f.value || "";
    if (!f.multiline) input.type = f.masked ? "password" : "text";
    if (f.multiline) input.rows = 8;
    input.id = "field-" + f.name;
    if (label) row.querySelector(".field-label").htmlFor = input.id;

    // Whole values, not keystrokes (webui.md §5.1). A frame per character is
    // unpleasant on a phone and outright broken with autocorrect and IME
    // composition, both of which revise runs rather than emitting keys.
    input.addEventListener("input", () => {
      send({ field: f.name, value: input.value });
    });
    // Enter submits a single-line field the way it does in the terminal;
    // in a textarea it is a newline, so leave it alone.
    if (!f.multiline) {
      input.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
          e.preventDefault();
          send({ field: f.name, value: input.value });
          key("enter");
        }
      });
    }
    row.appendChild(input);
    form.appendChild(row);

    if (f.hint) form.appendChild(el("div", "muted hint", f.hint.trim()));
    if (f.active) queueMicrotask(() => input.focus({ preventScroll: true }));
  }
  return form;
}

function renderChatLog(b) {
  const log = el("div", "block chatlog");
  if (!b.lines || b.lines.length === 0) {
    log.appendChild(el("div", "muted", b.empty || ""));
    return log;
  }
  for (const line of b.lines) {
    const row = el("div", line.system ? "chat-line system" : "chat-line");
    row.appendChild(el("span", "chat-time", line.time));
    if (line.system) {
      row.appendChild(el("span", "muted", "* " + line.text));
    } else {
      row.appendChild(el("span", "chat-nick", line.nick));
      row.appendChild(el("span", "chat-text", line.text));
    }
    log.appendChild(row);
  }
  return log;
}

function renderTabs(b) {
  const bar = el("div", "block tabs");
  (b.names || []).forEach((name, i) => {
    const tab = el("button", i === b.selected ? "tab selected" : "tab", name);
    // Tab is how the terminal cycles these, so stepping to the right one keeps
    // the two paths identical rather than adding a jump the SSH user lacks.
    tab.addEventListener("click", () => {
      let steps = (i - b.selected + b.names.length) % b.names.length;
      while (steps-- > 0) key("tab");
    });
    bar.appendChild(tab);
  });
  return bar;
}

function renderConfirm(b) {
  const box = el("div", "block confirm");
  box.appendChild(el("div", "confirm-question", b.question));
  const yes = el("button", "key danger", "Yes");
  yes.addEventListener("click", () => key(b.key || "y"));
  const no = el("button", "key", "No");
  no.addEventListener("click", () => key("n"));
  const row = el("div", "confirm-actions");
  row.append(yes, no);
  box.appendChild(row);
  return box;
}

const RENDERERS = {
  text: renderText,
  choices: renderChoices,
  table: renderTable,
  article: renderArticle,
  form: renderForm,
  chatlog: renderChatLog,
  tabs: renderTabs,
  confirm: renderConfirm,
};

// Every block carries its kind, so this is a lookup rather than a guess. An
// earlier version sniffed which fields were present, which fell apart exactly
// where you would expect — a table and a chat log are both lists of rows — and
// failed as a rendering bug instead of a decode error.
function renderBlock(b) {
  const fn = RENDERERS[b.kind];
  if (!fn) {
    console.warn("unknown block kind", b.kind);
    return el("div");
  }
  return fn(b);
}

// --- screen ----------------------------------------------------------------

// The last words the server said, kept so the disconnect notice can repeat
// them. A session cut short — a time limit, a sysop — must say why, or the
// caller assumes a fault and reconnects.
let farewell = "";

function renderScreen(msg) {
  if (msg.farewell) farewell = msg.farewell;
  const sc = msg.screen;
  const root = document.getElementById("screen");
  const atBottom =
    root.scrollHeight - root.scrollTop - root.clientHeight < 40;

  root.replaceChildren();
  root.dataset.kind = sc.kind || "";

  root.appendChild(el("h1", "title", sc.title || ""));
  root.appendChild(el("hr", "rule"));

  const body = el("div", "body");
  for (const b of sc.blocks || []) body.appendChild(renderBlock(b));
  root.appendChild(body);

  if (sc.status && sc.status.text) {
    root.appendChild(
      el("div", sc.status.isErr ? "status error" : "status", sc.status.text)
    );
  }

  const help = el("nav", "help");
  for (const h of sc.help || []) {
    if (!h.key) {
      help.appendChild(el("span", "help-note", h.label));
      continue;
    }
    // Buttons that keep their key label, so the web teaches the SSH interface
    // rather than replacing it: someone who learns `p post` here knows it there.
    const b = el("button", "key");
    b.appendChild(el("span", "hotkey", h.key));
    if (h.label) b.appendChild(el("span", "", h.label));
    b.addEventListener("click", () => key(h.key));
    help.appendChild(b);
  }
  root.appendChild(help);

  // Chat should stay pinned to the newest line unless the reader has
  // deliberately scrolled up to read back.
  if (sc.kind === "chat" && atBottom) root.scrollTop = root.scrollHeight;
}

// --- connection ------------------------------------------------------------

export function connect() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  socket = new WebSocket(`${proto}//${location.host}/ws`);

  socket.addEventListener("message", (e) => renderScreen(JSON.parse(e.data)));
  socket.addEventListener("close", () => {
    // A dropped connection ends the session, exactly as a dropped TCP
    // connection ends an SSH one. There is no resume: it would mean holding an
    // unlocked mail session open for a client that may never come back.
    document.getElementById("screen").replaceChildren(
      el("h1", "title", "Disconnected"),
      el("hr", "rule"),
      el("div", "muted", farewell || "Thanks for calling."),
      el("div", "muted", "Reload to reconnect.")
    );
  });
}

// Keys the page forwards wholesale, so the terminal's navigation works in a
// browser without every screen having to offer buttons for it.
const FORWARDED = new Set([
  "ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight",
  "Enter", "Escape", "Tab", "Backspace", "PageUp", "PageDown", "Home", "End",
]);

const NAMES = {
  ArrowUp: "up", ArrowDown: "down", ArrowLeft: "left", ArrowRight: "right",
  Enter: "enter", Escape: "escape", Tab: "tab", Backspace: "backspace",
  PageUp: "pgup", PageDown: "pgdown", Home: "home", End: "end",
};

document.addEventListener("keydown", (e) => {
  // Never steal keys from a field: the browser's own editing is most of why
  // typing here is nicer than typing over SSH.
  const tag = document.activeElement && document.activeElement.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA") return;

  if (e.ctrlKey && "cdu".includes(e.key.toLowerCase())) {
    // Ctrl+C must keep meaning copy in a browser; the BBS offers Quit instead.
    if (e.key.toLowerCase() === "c") return;
    e.preventDefault();
    key("ctrl+" + e.key.toLowerCase());
    return;
  }
  if (e.ctrlKey || e.metaKey || e.altKey) return;

  if (FORWARDED.has(e.key)) {
    e.preventDefault();
    key(NAMES[e.key]);
    return;
  }
  if (e.key.length === 1) {
    e.preventDefault();
    key(e.key);
  }
});
