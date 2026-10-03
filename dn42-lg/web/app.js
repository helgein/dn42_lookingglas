"use strict";

const SVGNS = "http://www.w3.org/2000/svg";
const W = 960, H = 540, CX = W / 2, CY = H / 2;
const nf = new Intl.NumberFormat("de-DE");
const $ = (id) => document.getElementById(id);

let current = null;
let topoSig = "";
let topoRefs = null;
const rowRefs = new Map();

// ---- helpers --------------------------------------------------------------

function el(tag, attrs = {}, parent = null, ns = null) {
  const e = ns ? document.createElementNS(ns, tag) : document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "text") e.textContent = v;
    else if (k === "class") e.setAttribute("class", v);
    else e.setAttribute(k, v);
  }
  if (parent) parent.appendChild(e);
  return e;
}
const sel = (tag, attrs, parent) => el(tag, attrs, parent, SVGNS);

function flash(node, cls = "chg") {
  node.classList.remove(cls);
  void node.getBoundingClientRect(); // restart animation
  node.classList.add(cls);
  clearTimeout(node._chgT);
  node._chgT = setTimeout(() => node.classList.remove(cls), 1500);
}

// Like "watch -d": set text, highlight when it changed.
function setText(node, value, highlight = true) {
  const v = String(value);
  if (node.textContent === v) return;
  const had = node._init;
  node.textContent = v;
  node._init = true;
  if (had && highlight) flash(node);
}

function shortName(p) { return p.name.replace(/^dn42_/, ""); }

function chan(p, name) {
  return (p.channels || []).find((c) => c.name === name) || null;
}

function fmtRate(r) {
  if (!r) return "0";
  return r < 10 ? r.toFixed(1).replace(".", ",") : nf.format(Math.round(r));
}

function isUp(p) {
  return p.state === "up" && (p.proto !== "BGP" || (p.bgpState || p.info || "").startsWith("Established"));
}

function bgpProtos(n) { return (n.protocols || []).filter((p) => p.kind === "ebgp" || p.kind === "ibgp"); }
function ebgp(n) { return (n.protocols || []).filter((p) => p.kind === "ebgp"); }

// ---- topology -------------------------------------------------------------

function routerPositions(n) {
  if (n === 1) return [{ x: CX, y: CY, out: null }];
  if (n === 2) return [
    { x: CX - 150, y: CY, out: Math.PI },
    { x: CX + 150, y: CY, out: 0 },
  ];
  const r = 150;
  return Array.from({ length: n }, (_, i) => {
    const a = -Math.PI / 2 + (i * 2 * Math.PI) / n;
    return { x: CX + r * Math.cos(a), y: CY + r * Math.sin(a), out: a };
  });
}

function buildTopo(st) {
  const svg = $("topo");
  svg.replaceChildren();
  const gHub = sel("g", {}, svg);
  const gEdges = sel("g", {}, svg);
  const gFlows = sel("g", {}, svg);
  const gNodes = sel("g", {}, svg);
  const refs = { routers: new Map(), peers: new Map(), ibgp: [] };
  const pos = routerPositions(st.nodes.length);

  const rx = st.nodes.length === 1 ? 120 : st.nodes.length === 2 ? 255 : 230;
  const ry = st.nodes.length <= 2 ? 100 : 230;
  sel("ellipse", { class: "t-hub", cx: CX, cy: CY, rx, ry }, gHub);
  refs.hubLabel = sel("text", { class: "t-hub-label", x: CX, y: CY - ry + 22, "text-anchor": "middle" }, gHub);

  // iBGP edges between routers
  for (let i = 0; i < pos.length; i++) {
    for (let j = i + 1; j < pos.length; j++) {
      const line = sel("line", {
        class: "t-edge ibgp", x1: pos[i].x, y1: pos[i].y, x2: pos[j].x, y2: pos[j].y, "stroke-width": 3,
      }, gEdges);
      refs.ibgp.push({ line, a: st.nodes[i].node, b: st.nodes[j].node });
    }
  }

  st.nodes.forEach((n, i) => {
    const p0 = pos[i];
    const peers = ebgp(n);
    const k = peers.length;
    peers.forEach((p, j) => {
      let a;
      if (p0.out === null) {
        a = -Math.PI / 2 + (j * 2 * Math.PI) / Math.max(k, 1) + (k === 2 ? Math.PI / 2 : 0);
      } else {
        const spread = k === 1 ? 0 : Math.min(Math.PI * 0.95, 0.6 * (k - 1));
        a = p0.out - spread / 2 + (k === 1 ? 0 : (j * spread) / (k - 1));
      }
      const dist = 205;
      const x = p0.x + dist * Math.cos(a);
      const y = p0.y + dist * Math.sin(a);
      const edge = sel("line", { class: "t-edge", x1: p0.x, y1: p0.y, x2: x, y2: y, "stroke-width": 2 }, gEdges);
      // flow runs from peer towards router: inbound updates
      const flow = sel("path", { class: "t-flow", d: `M${x},${y} L${p0.x},${p0.y}`, opacity: 0 }, gFlows);
      const g = sel("g", { class: "t-peer", tabindex: 0, role: "button" }, gNodes);
      const title = sel("title", {}, g);
      const circle = sel("circle", { cx: x, cy: y, r: 34 }, g);
      const cnt = sel("text", { class: "p-cnt", x, y: y + 4, "text-anchor": "middle" }, g);
      const ty = y + 54;
      sel("text", { class: "p-name", x, y: ty, "text-anchor": "middle", text: shortName(p) }, g);
      const sub = sel("text", { class: "p-sub", x, y: ty + 16, "text-anchor": "middle" }, g);
      const open = () => { if (p.neighborAs) runLookup("as", String(p.neighborAs), n.node); };
      g.addEventListener("click", open);
      g.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); open(); } });
      refs.peers.set(n.node + "|" + p.name, { g, edge, flow, cnt, sub, title });
    });

    const g = sel("g", { class: "t-router" }, gNodes);
    sel("rect", { x: p0.x - 78, y: p0.y - 30, width: 156, height: 60, rx: 6 }, g);
    sel("text", { class: "r-name", x: p0.x, y: p0.y - 4, "text-anchor": "middle", text: n.node }, g);
    const sub = sel("text", { class: "r-sub", x: p0.x, y: p0.y + 15, "text-anchor": "middle" }, g);
    refs.routers.set(n.node, { g, sub });
  });
  return refs;
}

function updateTopo(st) {
  const sig = st.nodes.map((n) => n.node + ":" + ebgp(n).map((p) => p.name).join(",")).join("|");
  let fit = false;
  if (sig !== topoSig) {
    topoRefs = buildTopo(st);
    topoSig = sig;
    fit = true;
  }
  const refs = topoRefs;
  refs.hubLabel.textContent = st.asn ? "AS" + st.asn : "";

  const ibgpUp = new Map();
  for (const n of st.nodes) {
    const r = refs.routers.get(n.node);
    if (r) {
      r.g.classList.toggle("down", !n.ok);
      r.sub.textContent = n.ok ? (n.status.routerId || "") : "nicht erreichbar";
    }
    const ib = (n.protocols || []).filter((p) => p.kind === "ibgp");
    ibgpUp.set(n.node, n.ok && ib.length > 0 && ib.every(isUp));

    for (const p of ebgp(n)) {
      const ref = refs.peers.get(n.node + "|" + p.name);
      if (!ref) continue;
      const up = n.ok && isUp(p);
      ref.g.classList.toggle("down", !up);
      ref.edge.classList.toggle("down", !up);
      const r = up ? p.rateIn : 0;
      const w = 2 + Math.min(7, Math.log2(1 + r) * 1.6);
      ref.edge.setAttribute("stroke-width", w.toFixed(1));
      ref.flow.setAttribute("opacity", r > 0 ? Math.min(1, 0.35 + Math.log2(1 + r) * 0.2).toFixed(2) : "0");
      ref.flow.setAttribute("stroke-width", Math.max(2, w * 0.45).toFixed(1));
      const v4 = chan(p, "ipv4"), v6 = chan(p, "ipv6");
      const total = (v4 ? v4.imported : 0) + (v6 ? v6.imported : 0);
      const txt = up ? nf.format(total) : (p.state === "up" ? (p.bgpState || "–") : p.state);
      if (ref.cnt.textContent !== txt && ref.cnt._init) flash(ref.g);
      ref.cnt.textContent = txt;
      ref.cnt._init = true;
      ref.sub.textContent = p.neighborAs ? "AS" + p.neighborAs : "";
      ref.title.textContent =
        `${p.name} auf ${n.node}\n${p.bgpState || p.info || p.state}\n` +
        `IPv4: ${v4 ? nf.format(v4.imported) + " rein, " + nf.format(v4.exported) + " raus" : "–"}\n` +
        `IPv6: ${v6 ? nf.format(v6.imported) + " rein, " + nf.format(v6.exported) + " raus" : "–"}\n` +
        `Updates/s: ${fmtRate(p.rateIn)} rein, ${fmtRate(p.rateOut)} raus`;
    }
  }
  for (const e of refs.ibgp) {
    const up = ibgpUp.get(e.a) && ibgpUp.get(e.b);
    e.line.classList.toggle("down", !up);
  }
  if (fit) {
    const svg = $("topo");
    const bb = svg.getBBox();
    const pad = 16;
    svg.setAttribute("viewBox", `${Math.floor(bb.x - pad)} ${Math.floor(bb.y - pad)} ${Math.ceil(bb.width + 2 * pad)} ${Math.ceil(bb.height + 2 * pad)}`);
  }
}

// ---- router summary -------------------------------------------------------

function updateNodes(st) {
  const box = $("nodes");
  box.replaceChildren();
  for (const n of st.nodes) {
    const d = el("div", { class: "n" + (n.ok ? "" : " bad") }, box);
    el("strong", { text: n.node }, d);
    if (!n.ok) {
      d.append(" ");
      el("span", { class: "err", text: n.error || "keine Daten" }, d);
      continue;
    }
    const s = n.status || {};
    const bits = [];
    if (s.version) bits.push("BIRD " + s.version);
    if (s.routerId) bits.push("Router-ID " + s.routerId);
    if (s.lastReconfig) bits.push("rekonfiguriert " + s.lastReconfig);
    d.append(" " + bits.join(", "));
  }
}

// ---- sessions table -------------------------------------------------------

function spark(svg, a, b) {
  const max = Math.max(1, ...a, ...b);
  const pts = (arr) => arr.map((v, i) => {
    const x = (i / Math.max(1, 89)) * 140;
    const y = 22 - (v / max) * 20;
    return x.toFixed(1) + "," + y.toFixed(1);
  }).join(" ");
  svg.children[0].setAttribute("points", pts(b));
  svg.children[1].setAttribute("points", pts(a));
}

function updateSessions(st) {
  const tbody = $("sessions").tBodies[0];
  const seen = new Set();
  for (const n of st.nodes) {
    for (const p of bgpProtos(n)) {
      const key = n.node + "|" + p.name;
      seen.add(key);
      let r = rowRefs.get(key);
      if (!r) {
        const tr = el("tr", {}, tbody);
        r = { tr };
        r.node = el("td", {}, tr);
        r.sess = el("td", { class: "sess" }, tr);
        r.nb = el("td", { class: "nb" }, tr);
        r.state = el("td", { class: "state" }, tr);
        r.since = el("td", {}, tr);
        r.v4 = el("td", { class: "num" }, tr);
        r.v6 = el("td", { class: "num" }, tr);
        r.rate = el("td", { class: "num" }, tr);
        const td = el("td", {}, tr);
        r.spark = sel("svg", { class: "spark", viewBox: "0 0 140 24", "aria-hidden": "true" }, td);
        sel("polyline", { class: "out" }, r.spark);
        sel("polyline", {}, r.spark);
        r.node.textContent = n.node;
        r.sess.textContent = p.name;
        r.nbAs = el("span", { class: "asn" }, r.nb);
        rowRefs.set(key, r);
      }
      const up = n.ok && isUp(p);
      r.tr.classList.toggle("down", !up);
      r.tr.classList.toggle("ibgp", p.kind === "ibgp");
      setText(r.nbAs, p.neighborAs ? "AS" + p.neighborAs : "", false);
      r.nb.title = p.neighborAddr || "";
      setText(r.state, n.ok ? (p.bgpState || p.info || p.state) : "veraltet");
      setText(r.since, p.since);
      const io = (c) => c ? nf.format(c.imported) + " / " + nf.format(c.exported) : "–";
      setText(r.v4, io(chan(p, "ipv4")));
      setText(r.v6, io(chan(p, "ipv6")));
      setText(r.rate, fmtRate(p.rateIn) + " / " + fmtRate(p.rateOut));
      spark(r.spark, p.histIn || [], p.histOut || []);
    }
  }
  for (const [k, r] of rowRefs) {
    if (!seen.has(k)) { r.tr.remove(); rowRefs.delete(k); }
  }
}

// ---- own prefixes ---------------------------------------------------------

function updateOwn(st) {
  const cols = [];
  const prefixes = new Set();
  for (const n of st.nodes) {
    for (const p of ebgp(n)) cols.push({ node: n.node, proto: p.name, label: n.node + " → " + shortName(p) });
    for (const o of n.own || []) prefixes.add(o.prefix);
  }
  const wrap = $("own-wrap");
  if (prefixes.size === 0) { wrap.hidden = true; return; }
  wrap.hidden = false;
  const t = $("own");
  const prev = t._cells || new Map();
  t.replaceChildren();
  const head = el("tr", {}, el("thead", {}, t));
  el("th", { scope: "col", text: "Präfix" }, head);
  for (const c of cols) el("th", { scope: "col", text: c.label }, head);
  const body = el("tbody", {}, t);
  const cells = new Map();
  for (const pfx of prefixes) {
    const tr = el("tr", {}, body);
    el("td", { class: "pfx", text: pfx }, tr);
    for (const c of cols) {
      const n = st.nodes.find((x) => x.node === c.node);
      const o = (n.own || []).find((x) => x.prefix === pfx);
      const v = o && c.proto in o.announced ? (o.announced[c.proto] ? "ja" : "nein") : "–";
      const td = el("td", { class: v === "ja" ? "yes" : v === "nein" ? "no" : "", text: v }, tr);
      const key = pfx + "|" + c.node + "|" + c.proto;
      if (prev.has(key) && prev.get(key) !== v) flash(td);
      cells.set(key, v);
    }
  }
  t._cells = cells;
}

// ---- lookup ---------------------------------------------------------------

function updateNodeSelect(st) {
  const s = $("node");
  const names = st.nodes.map((n) => n.node);
  if (s._names === names.join("|")) return;
  const keep = s.value;
  s.replaceChildren();
  for (const nm of names) el("option", { value: nm, text: nm }, s);
  if (names.includes(keep)) s.value = keep;
  s._names = names.join("|");
}

function escapeHtml(s) {
  return s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

function highlight(text, ownAsn) {
  const asLink = (num) => {
    const cls = String(num) === String(ownAsn) ? "as own-as" : "as";
    return `<a href="#" class="${cls}" data-asn="${num}">${num}</a>`;
  };
  return text.split("\n").map((line) => {
    let h = escapeHtml(line);
    if (/BGP\.as_path:/.test(line)) {
      h = h.replace(/(BGP\.as_path:)(.*)$/, (_, k, v) => k + v.replace(/\b(\d{4,10})\b/g, (m) => asLink(m)));
    } else {
      h = h.replace(/\[AS(\d+)([ie?])\]/g, (_, num, o) => "[AS" + asLink(num) + o + "]");
      h = h.replace(/^(\S+\/\d+)(\s)/, '<span class="net">$1</span>$2');
    }
    return h;
  }).join("\n");
}

async function runLookup(mode, q, node) {
  const out = $("out"), cmd = $("cmd");
  if (q !== undefined) $("q").value = q;
  if (node) $("node").value = node;
  q = $("q").value.trim();
  node = $("node").value;
  if (!q) { $("q").focus(); return; }
  if (mode === "for" && /^(AS)?\d+$/i.test(q)) mode = "as";
  cmd.textContent = "Frage " + node + " …";
  out.textContent = "";
  try {
    const r = await fetch("/api/route?" + new URLSearchParams({ mode, q, node }));
    const res = await r.json();
    cmd.replaceChildren();
    if (res.commands && res.commands.length) {
      cmd.append(res.node + ": ");
      res.commands.forEach((c, i) => {
        if (i) cmd.append(", ");
        el("code", { text: c }, cmd);
      });
    }
    if (res.error) {
      if (cmd.childNodes.length) cmd.append(" ");
      el("span", { class: "err", text: res.error }, cmd);
    }
    let txt = res.output || "";
    if (!txt.trim() && !res.error) txt = "Keine Route gefunden.";
    if (res.truncated) txt += "\n… gekürzt auf 1000 Zeilen.";
    out.innerHTML = highlight(txt, current ? current.asn : 0);
    out.scrollTop = 0;
  } catch (e) {
    cmd.replaceChildren();
    el("span", { class: "err", text: "Abfrage fehlgeschlagen: " + e.message }, cmd);
  }
}

$("lookup-form").addEventListener("submit", (e) => { e.preventDefault(); runLookup("for"); });
$("btn-as").addEventListener("click", () => runLookup("as"));
$("out").addEventListener("click", (e) => {
  const a = e.target.closest("a.as");
  if (!a) return;
  e.preventDefault();
  runLookup("as", a.dataset.asn);
});

// ---- live stream ----------------------------------------------------------

function render(st) {
  current = st;
  const asTxt = st.asn ? "AS" + st.asn : "Looking Glass";
  $("asn").textContent = asTxt;
  const sub = st.title || st.nodes.map((n) => n.node).join(" und ");
  $("title").textContent = sub;
  document.title = asTxt + " Looking Glass";
  updateTopo(st);
  updateNodes(st);
  updateSessions(st);
  updateOwn(st);
  updateNodeSelect(st);
  const live = $("live");
  live.className = "live " + (st.nodes.every((n) => n.ok) ? "ok" : "bad");
  const t = new Date(st.generated).toLocaleTimeString("de-DE");
  const bad = st.nodes.filter((n) => !n.ok).map((n) => n.node);
  live.textContent = bad.length ? `Live, Stand ${t}, ohne Daten von ${bad.join(", ")}` : `Live, Stand ${t}`;
}

function connect() {
  const es = new EventSource("/api/stream");
  es.onmessage = (ev) => {
    try { render(JSON.parse(ev.data)); } catch (e) { console.error(e); }
  };
  es.onerror = () => {
    const live = $("live");
    live.className = "live bad";
    live.textContent = "Keine Verbindung zum Looking Glass, neuer Versuch läuft";
  };
}

connect();
