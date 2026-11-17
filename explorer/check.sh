#!/usr/bin/env bash
# Checks the explorer app in a headless Chrome (work orders X2, X3): builds
# the site, opens it from its file:// URL, visits every route of the app's
# views in light and dark at 1280 pixels (and a few at 1920), saves a
# screenshot of each, fails on any console error or uncaught exception,
# and runs assertions in the page: search finds a symbol and a concern,
# following a link in the source lands on the definition, the inspector's
# back button works, a package page lists its files, stepping through a
# flow changes the step, the URL and the inspected code, a reference in a
# concern's text opens the inspector, the runtime map draws a connector
# for every channel, a term shows its definition on hover, the findings
# lead to code, the source marks flow steps (one mark per kind on a line),
# the flows index filters, the glossary's index selects a term, a concern's
# side column starts with "On this page" and follows the reading, a wide
# table says it scrolls, loops show their labels, a flow opened at a step
# shows it, tour steps name their code once, unchecked reasons and weaker
# paths are shown, the filtered runtime map names its channels, and the
# app, the largest file, the largest document and the runtime map are fast. Nothing about the
# code base is written here: the routes and the things to look for are
# chosen from the site's data.
#
#   explorer/check.sh [--no-build] [--site dir] [--shot '#/route']... [--full] [output-directory]
#
#   --no-build   check explorer/dist as it is
#   --site dir   check the site in dir (built with --out); implies --no-build
#   --shot frag  only take screenshots of this route (repeatable), in both
#                themes at 1280 and 1920 pixels, and report console errors;
#                for authors previewing their pages
#   --full       with --shot: capture the whole page, not only the window
#                (the window grows until the page and the scrolling parts
#                of the view, such as a flow's diagram, show everything;
#                at most 16000 pixels tall)
#
# Needs python3 (standard library only: a minimal DevTools protocol client
# below) and Chrome or Chromium: $CHROME, else chrome-headless-shell or
# chrome under ~/.local/opt, else chromium or google-chrome on the PATH.
# Everything it starts is stopped on exit.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
BUILD=1
OUT_ARG=""
SITE_DIR="$REPO/explorer/dist"
SHOTS=()
FULL=0
while [ "$#" -gt 0 ]; do
	case "$1" in
	--no-build) BUILD=0; shift ;;
	--full) FULL=1; shift ;;
	--site) SITE_DIR="$(cd "$2" && pwd -P)"; BUILD=0; shift 2 ;;
	--shot) SHOTS+=("$2"); shift 2 ;;
	*) OUT_ARG="$1"; shift ;;
	esac
done
OUT="${OUT_ARG:-$(mktemp -d)}"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd -P)"

find_chrome() {
	if [ -n "${CHROME:-}" ]; then echo "$CHROME"; return; fi
	local c
	for c in "$HOME"/.local/opt/chrome-for-testing/*/chrome-headless-shell-linux64/chrome-headless-shell \
		"$HOME"/.local/opt/chrome-for-testing/*/chrome-linux64/chrome; do
		[ -x "$c" ] && { echo "$c"; return; }
	done
	for c in chromium chromium-browser google-chrome chrome; do
		command -v "$c" >/dev/null 2>&1 && { command -v "$c"; return; }
	done
	echo "explorer/check.sh: no Chrome or Chromium found (set CHROME)" >&2
	exit 1
}
CHROME_BIN="$(find_chrome)"

if [ "$BUILD" = 1 ]; then
	"$REPO/dev" explore >"$OUT/build.log" 2>&1 || { tail -20 "$OUT/build.log" >&2; echo "explorer/check.sh: the build failed" >&2; exit 1; }
fi
SITE="$SITE_DIR/index.html"
[ -f "$SITE" ] || { echo "explorer/check.sh: $SITE is missing (run ./dev explore)" >&2; exit 1; }

WORK="$(mktemp -d)"
CHROME_PID=""
cleanup() {
	[ -n "$CHROME_PID" ] && kill "$CHROME_PID" 2>/dev/null && wait "$CHROME_PID" 2>/dev/null || true
	rm -rf "${WORK:?}"
}
trap cleanup EXIT

CDP_PORT="$(python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
"$CHROME_BIN" --headless --no-sandbox --disable-gpu --hide-scrollbars --no-first-run --no-default-browser-check \
	--user-data-dir="$WORK/profile" --remote-debugging-address=127.0.0.1 --remote-debugging-port="$CDP_PORT" \
	about:blank >"$OUT/chrome.log" 2>&1 &
CHROME_PID=$!

cat >"$WORK/cdp.py" <<'PY'
import base64, json, os, socket, struct, sys, time, urllib.request

site, cdp, out, full = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4] == "1"
only = sys.argv[5:]
base = "file://" + site

def wait_http(url, tries=100):
    for _ in range(tries):
        try:
            return urllib.request.urlopen(url, timeout=2).read()
        except Exception:
            time.sleep(0.2)
    raise SystemExit(f"no answer from {url}")

class Page:
    """A minimal client of the DevTools protocol over a WebSocket."""
    def __init__(self, ws_url):
        host, rest = ws_url[len("ws://"):].split("/", 1)
        h, p = host.split(":")
        self.s = socket.create_connection((h, int(p)))
        key = base64.b64encode(os.urandom(16)).decode()
        self.s.sendall((f"GET /{rest} HTTP/1.1\r\nHost: {host}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                        f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n").encode())
        buf = b""
        while b"\r\n\r\n" not in buf:
            buf += self.s.recv(4096)
        self.buf = buf.split(b"\r\n\r\n", 1)[1]
        self.n = 0
        self.events = []
    def _read(self, n):
        while len(self.buf) < n:
            chunk = self.s.recv(1 << 20)
            if not chunk:
                raise SystemExit("DevTools connection closed")
            self.buf += chunk
        b, self.buf = self.buf[:n], self.buf[n:]
        return b
    def _recv(self):
        data = b""
        while True:
            b1, b2 = self._read(2)
            n = b2 & 0x7F
            if n == 126: n = struct.unpack(">H", self._read(2))[0]
            elif n == 127: n = struct.unpack(">Q", self._read(8))[0]
            data += self._read(n)
            if b1 & 0x80:
                return json.loads(data)
    def send(self, method, params=None, timeout=60):
        self.n += 1
        msg = json.dumps({"id": self.n, "method": method, "params": params or {}}).encode()
        hdr = bytes([0x81])
        if len(msg) < 126: hdr += bytes([0x80 | len(msg)])
        elif len(msg) < 65536: hdr += bytes([0x80 | 126]) + struct.pack(">H", len(msg))
        else: hdr += bytes([0x80 | 127]) + struct.pack(">Q", len(msg))
        mask = os.urandom(4)
        self.s.sendall(hdr + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(msg)))
        end = time.time() + timeout
        while time.time() < end:
            m = self._recv()
            if m.get("id") == self.n:
                if "error" in m:
                    raise SystemExit(f"{method}: {m['error']}")
                return m.get("result", {})
            self.events.append(m)
        raise SystemExit(f"{method}: no answer")
    def js(self, expr):
        r = self.send("Runtime.evaluate", {"expression": expr, "returnByValue": True, "awaitPromise": True})
        if "exceptionDetails" in r:
            raise SystemExit(f"evaluating {expr[:200]!r}: {r['exceptionDetails']}")
        return r["result"].get("value")
    def wait(self, expr, what, timeout=20):
        end = time.time() + timeout
        while time.time() < end:
            r = self.send("Runtime.evaluate", {"expression": f"(() => {{ try {{ return !!({expr}); }} catch (e) {{ return false; }} }})()", "returnByValue": True})
            if r.get("result", {}).get("value"):
                return
            time.sleep(0.1)
        raise SystemExit(f"timed out waiting for {what}")
    def goto(self, frag):
        # Through about:blank, so that a change of the fragment alone is a
        # new page load too.
        self.send("Page.navigate", {"url": "about:blank"})
        self.wait("location.href === 'about:blank'", "about:blank")
        self.send("Page.navigate", {"url": base + frag})
        self.wait("document.documentElement.dataset.app === 'started'", f"the app to start at {frag}")
    def shot(self, name):
        open(os.path.join(out, name + ".png"), "wb").write(base64.b64decode(self.send("Page.captureScreenshot", {"format": "png"})["data"]))

wait_http(f"http://127.0.0.1:{cdp}/json/version")
req = urllib.request.Request(f"http://127.0.0.1:{cdp}/json/new?about:blank", method="PUT")
target = json.loads(urllib.request.urlopen(req).read())
page = Page(target["webSocketDebuggerUrl"])
for d in ("Page", "Runtime", "Log"):
    page.send(d + ".enable")
page.send("Emulation.setFocusEmulationEnabled", {"enabled": True})

problems = []
def collect(label):
    for m in page.events:
        meth, p = m.get("method"), m.get("params", {})
        if meth == "Runtime.exceptionThrown":
            d = p["exceptionDetails"]
            problems.append(f"{label}: exception: {d.get('exception', {}).get('description') or d.get('text')}")
        elif meth == "Runtime.consoleAPICalled" and p.get("type") in ("error", "assert"):
            problems.append(f"{label}: console.{p['type']}: " + " ".join(str(a.get('value', a.get('description', ''))) for a in p.get("args", [])))
        elif meth == "Log.entryAdded" and p["entry"].get("level") == "error":
            problems.append(f"{label}: {p['entry'].get('source')}: {p['entry'].get('text')} {p['entry'].get('url', '')}")
    page.events = []

def size(w, h, scheme):
    page.send("Emulation.setDeviceMetricsOverride", {"width": w, "height": h, "deviceScaleFactor": 1, "mobile": False})
    page.send("Emulation.setEmulatedMedia", {"features": [{"name": "prefers-color-scheme", "value": scheme}]})

# How much taller the window must be for the view to show everything: the
# largest overflow of a scrolling element of the view (the inspector and
# the code view's own panes are left as they are).
OVERFLOW_JS = """(() => {
  let most = 0;
  document.querySelectorAll('.view, .view *').forEach(e => {
    if (e.closest('.view-code, .insp')) return;
    const cs = getComputedStyle(e);
    if (!/(auto|scroll)/.test(cs.overflowY) || !e.clientHeight || e.offsetParent === null && e !== document.querySelector('.view')) return;
    most = Math.max(most, e.scrollHeight - e.clientHeight);
  });
  return most;
})()"""

def grow(w, h):
    """Grows the window until the view no longer scrolls, at most 16000 pixels."""
    for _ in range(6):
        more = page.js(OVERFLOW_JS) or 0
        if more <= 1 or h >= 16000:
            break
        h = min(16000, h + more)
        page.send("Emulation.setDeviceMetricsOverride", {"width": w, "height": h, "deviceScaleFactor": 1, "mobile": False})
        time.sleep(0.4)
    return h

# The check must be able to see an error: provoke one.
size(1280, 800, "light")
page.goto("#/overview")
page.js("setTimeout(function () { throw new Error('deliberate'); }, 0); 1")
time.sleep(0.3)
page.js("1")
collect("self-test")
if not any("deliberate" in p for p in problems):
    raise SystemExit("the check did not notice a deliberate exception: " + repr(problems))
problems.clear()

# Only screenshots of the routes asked for.
if only:
    for frag in only:
        frag = frag if frag.startswith("#") else "#" + frag
        name = "".join(ch if ch.isalnum() else "-" for ch in frag.strip("#/"))[:80].strip("-") or "root"
        for scheme in ("light", "dark"):
            for w, h in ((1280, 800), (1920, 1080)):
                size(w, h, scheme)
                page.goto(frag)
                time.sleep(0.8)
                if full:
                    grow(w, h)
                page.shot(f"{name}-{w}-{scheme}" + ("-full" if full else ""))
                collect(f"{frag} {w} {scheme}")
    print(f"{len(only) * 4} screenshots in {out}")
    if problems:
        print("problems:")
        for p in problems:
            print("  " + p)
        sys.exit(1)
    print("no errors or exceptions")
    sys.exit(0)

# Choose what to look at from the data: the extremes.
pick = page.js("""(() => {
  const c = X.data.core, by = (a, f) => a.slice().sort((x, y) => f(y) - f(x))[0];
  const lines = f => f.lines.code + f.lines.comment + f.lines.blank;
  const goFiles = c.files.filter(f => f.lang === 'go' && !f.ignored);
  const big = by(goFiles, lines);
  const other = by(c.files.filter(f => f.lang !== 'go' && f.lang !== 'binary' && f.chunk), lines);
  const pkg = by(c.packages, p => p.files.length);
  const used = by(c.symbols.filter(s => (s.kind === 'func' || s.kind === 'type') && s.exported && !s.test), s => s.uses || 0);
  const fn = by(c.symbols.filter(s => s.kind === 'method' && s.exported && !s.test && s.doc), s => (s.endLine - s.line) + (s.uses || 0));
  const iface = c.symbols.find(s => s.typeKind === 'interface' && c.implementations.some(i => i.interface === s.ref));
  const cmd = by(c.commands, x => (x.flags || []).length);
  const cfg = by(c.config.filter(k => k.kind === 'value'), k => ((X.data.sym(k.field) || {}).uses || 0));
  const M = X.data.model;
  const flowsF = (M.flows || []).filter(f => f.kind !== 'tour' && X.model.steps(f).length > 1);
  const longFlow = flowsF.length ? by(flowsF, f => X.model.steps(f).length) : null;
  const tour = (M.flows || []).find(f => f.kind === 'tour');
  const concern = (M.concerns || []).length ? by(M.concerns, c => (c.diagrams || []).length * 100000 + (c.body || '').length) : null;
  const actor = (M.actors || []).length ? by(M.actors, a => (M.channels || []).filter(x => x.from === a.id || x.to === a.id).length) : null;
  const bch = (M.channels || []).find(x => x.boundary) || (M.channels || [])[0];
  const finding = c.findings.length ? by(c.findings, f => (f.cited || []).length) : null;
  const doc = c.documents.length ? by(c.documents, d => d.bytes) : null;
  const termConcern = (M.concerns || []).find(x => /\\[\\[term:/.test(x.body || ''));
  let stepSym = null;
  (M.flows || []).forEach(f => X.model.steps(f).forEach(st => { const s = st.step.ref && X.data.sym(st.step.ref); if (!stepSym && s && s.kind !== 'field') stepSym = s; }));
  return {
    flow: longFlow ? longFlow.id : '', flowSteps: longFlow ? X.model.steps(longFlow).length : 0, tour: tour ? tour.id : '',
    concern: concern ? concern.id : '', diagram: concern && (concern.diagrams || []).length ? concern.diagrams[0].id : '',
    actor: actor ? actor.id : '', channel: bch ? bch.id : '', term: (M.terms || []).length ? M.terms[0].id : '',
    finding: finding ? finding.id : '', findingCited: finding ? (finding.cited || []).length : 0, doc: doc ? doc.path : '',
    termConcern: termConcern ? termConcern.id : '', stepFile: stepSym ? stepSym.file : '', stepLine: stepSym ? stepSym.line : 0,
    channels: (M.channels || []).filter(x => X.data.actor(x.from) && X.data.actor(x.to)).length,
    big: big.path, bigLines: lines(big), other: other ? other.path : '', pkg: pkg.path, pkgFiles: pkg.files.length,
    used: used.ref, usedName: used.name, fn: fn.ref, iface: iface ? iface.ref : '', cmd: cmd.path, cfg: cfg ? cfg.key : '',
    rpc: c.rpc.length ? c.rpc[0].name : '', dir: pkg.path.split('/')[0],
    surfaces: (X.data.model.surfaces || []).map(s => s.id)
  };
})()""")

routes = [
    ("overview", "#/overview", ".ov-text"),
    ("runtime", "#/runtime", ".rt-map .rt-actor"),
    ("concerns", "#/concerns", ".page h1"),
    ("flows", "#/flows", ".page h1"),
    ("state", "#/state", ".page h1"),
    ("glossary", "#/glossary", ".page h1"),
    ("docs", "#/docs", ".dl-i"),
    ("findings", "#/docs/findings", ".fd-table tbody tr"),
    ("packages", "#/packages", ".layers .ptile"),
    ("packages-zone", "#/packages?color=zone&all=1", ".layers .ptile"),
    ("packages-tests", "#/packages?color=tests", ".layers .ptile"),
    ("size", "#/packages/size", ".treemap .tm-f"),
    ("package", f"#/pkg/{pick['pkg']}", ".pp-files tbody tr"),
    ("code", "#/code", ".tree .tn"),
    ("code-dir", f"#/code/{pick['dir']}", ".dir-list"),
    ("code-big", f"#/code/{pick['big']}?line={pick['bigLines'] // 2}", ".src .r.sel"),
    ("code-symbol", f"#/code/?sym={pick['used']}", ".cv-sym .sp-dyn .sp-sec"),
    ("code-method", f"#/code/?sym={pick['fn']}", ".cv-sym .sp-dyn .sp-sec"),
    ("insp-symbol", f"#/packages?i={pick['used']}", ".insp-body .sp-dyn .sp-sec"),
    ("insp-package", f"#/packages?i=pkg:{pick['pkg']}", ".insp-body .pkgsum"),
    ("insp-command", f"#/interfaces/{pick['surfaces'][0] if pick['surfaces'] else ''}?i=cmd:{pick['cmd']}", ".insp-body .sp-sig"),
    ("insp-config", f"#/packages?i=config:{pick['cfg']}", ".insp-body .cfg-uses .uses, .insp-body .cfg-uses p"),
    ("insp-file", f"#/packages?i=file:{pick['big']}", ".insp-body .src .r"),
]
if pick["other"]:
    routes.append(("code-other", f"#/code/{pick['other']}", ".src .r"))
if pick["actor"]:
    routes.append(("runtime-actor", f"#/runtime?i=actor:{pick['actor']}", ".insp-body .ins-head"))
if pick["channel"]:
    routes.append(("runtime-channel", f"#/runtime?i=channel:{pick['channel']}", ".insp-body .ins-ends"))
if pick["flow"]:
    routes.append(("runtime-flow", f"#/runtime?flow={pick['flow']}", ".rt-map.filtered"))
    routes.append(("flow", f"#/flows/{pick['flow']}?step={max(1, pick['flowSteps'] // 2)}", ".fp-step .fs-nav"))
    routes.append(("insp-flow", f"#/flows?i=flow:{pick['flow']}", ".insp-body .fl-list"))
if pick["tour"]:
    routes.append(("tour", f"#/flows/{pick['tour']}", ".fl-step.cur"))
if pick["concern"]:
    routes.append(("concern", f"#/concerns/{pick['concern']}", ".cp-body"))
    routes.append(("insp-concern", f"#/concerns?i=concern:{pick['concern']}", ".insp-body .lp"))
    if pick["diagram"]:
        routes.append(("concern-diagram", f"#/concerns/{pick['concern']}?diagram={pick['diagram']}", ".gd-fig .gd-node"))
if pick["term"]:
    routes.append(("insp-term", f"#/glossary?sel={pick['term']}&i=term:{pick['term']}", ".insp-body .md"))
if pick["finding"]:
    routes.append(("insp-finding", f"#/docs/findings?sel={pick['finding']}&i=finding:{pick['finding']}", ".insp-body .ins-title"))
if pick["doc"]:
    routes.append(("doc", f"#/docs/{pick['doc']}", ".dv-md .md-h"))
if pick["stepFile"]:
    routes.append(("code-marks", f"#/code/{pick['stepFile']}?line={pick['stepLine']}", ".src .mkx"))
if pick["iface"]:
    routes.append(("code-interface", f"#/code/?sym={pick['iface']}", ".cv-sym .sp-dyn"))
if pick["rpc"]:
    routes.append(("insp-rpc", f"#/packages?i=rpc:{pick['rpc']}", ".insp-body .sp-sec"))
for s in pick["surfaces"]:
    routes.append((f"interfaces-{s}", f"#/interfaces/{s}", ".xt-table tbody tr"))

wide = {"packages", "code-big", "code-symbol", "insp-symbol", "package", "size", "interfaces-" + (pick["surfaces"][0] if pick["surfaces"] else ""),
        "overview", "runtime", "runtime-actor", "flow", "concern", "concern-diagram", "doc", "tour"}
count = 0
for scheme in ("light", "dark"):
    for name, frag, ready in routes:
        for w, h in ((1280, 800), (1920, 1080)):
            if w == 1920 and name not in wide:
                continue
            label = f"{name}-{w}-{scheme}"
            size(w, h, scheme)
            page.goto(frag.replace("#/code/?sym=", "#/code?sym="))
            page.wait(f"document.querySelector({json.dumps(ready)})", f"{label}: {ready}")
            time.sleep(0.35)
            page.shot(label)
            collect(label)
            count += 1

# ----- assertions
size(1280, 800, "light")
page.send("Emulation.setEmulatedMedia", {"features": [{"name": "prefers-color-scheme", "value": "light"}]})
failures = []
def check(ok, what):
    print(("ok    " if ok else "FAIL  ") + what)
    if not ok:
        failures.append(what)

# Search finds a known symbol, by its name, through the dialog.
page.goto("#/overview")
page.js("document.dispatchEvent(new KeyboardEvent('keydown', {key: '/', bubbles: true}))")
page.wait("document.querySelector('.modal.search.on')", "the search dialog")
page.js(f"""(() => {{ const i = document.querySelector('.sr-input'); i.value = {json.dumps(pick['usedName'])}; i.dispatchEvent(new Event('input')); }})()""")
page.wait("document.querySelector('.sr-row')", "search results")
top = page.js("[...document.querySelectorAll('.sr-row')].slice(0, 3).map(r => r.dataset.ref)")
check(pick["used"] in (top or []), f"search for {pick['usedName']!r} lists {pick['used']} among the first results ({top})")
page.shot("search-names")
page.js("document.querySelector('.sr-input').dispatchEvent(new KeyboardEvent('keydown', {key: 'Enter', bubbles: true}))")
page.wait("location.hash.indexOf('#/code/') === 0", "the search result to open")
collect("search")

# The text of the code: Tab switches, every chunk loads, lines match.
page.js("document.dispatchEvent(new KeyboardEvent('keydown', {key: 'k', ctrlKey: true, bubbles: true}))")
page.wait("document.querySelector('.modal.search.on')", "the search dialog again")
page.js("document.querySelector('.sr-input').dispatchEvent(new KeyboardEvent('keydown', {key: 'Tab', bubbles: true}))")
page.js(f"""(() => {{ const i = document.querySelector('.sr-input'); i.value = {json.dumps(pick['usedName'] + '{')}; i.dispatchEvent(new Event('input')); }})()""")
page.wait("document.querySelector('.sr-row.tl')", "lines of text found", 60)
status = page.js("document.querySelector('.sr-status').textContent")
check(bool(status), f"the text search finds lines ({status})")
page.shot("search-text")
page.js("document.querySelector('.sr-input').dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape', bubbles: true}))")
collect("text search")

# Following a link in the source lands on the definition.
page.goto(f"#/code/{pick['big']}")
page.wait("document.querySelector('.cv-src .src .r')", "the source")
target = page.js(f"""(() => {{
  const here = {json.dumps(pick['big'])};
  const a = [...document.querySelectorAll('.cv-src a.sl')].find(a => {{ const s = X.data.sym(a.dataset.ref); return s && s.file !== here && s.kind !== 'field'; }});
  if (!a) return null;
  a.scrollIntoView(); a.click();
  return a.dataset.ref;
}})()""")
check(target is not None, "the large file has a link to another file")
if target:
    sym = page.js(f"X.data.sym({json.dumps(target)})")
    page.wait(f"location.hash.indexOf('#/code/' + {json.dumps(sym['file'])}) === 0 && document.querySelector('.cv-src .src .r.in')", "the definition")
    rng = page.js("[...document.querySelectorAll('.cv-src .src .r.in')].map(r => +r.dataset.n)")
    title = page.js("(document.querySelector('.cv-sym h2') || {}).textContent || ''")
    check(sym["line"] in rng and title.endswith(sym["name"]), f"following {target} shows {sym['file']}:{sym['line']} with its symbol panel")
    page.js("history.back()")
    page.wait(f"location.hash.indexOf('#/code/' + {json.dumps(pick['big'])}) === 0", "back to the first file")
collect("source links")

# A hover card appears over an identifier of the source.
page.wait("document.querySelector('.cv-src .src a.sl')", "the source again")
time.sleep(0.3)
pos = page.js("""(() => { const a = [...document.querySelectorAll('.cv-src a.sl')].find(a => X.data.sym(a.dataset.ref));
  if (!a) return null; a.scrollIntoView({block: 'center'}); const r = a.getBoundingClientRect(); return [r.left + r.width / 2, r.top + r.height / 2, a.dataset.ref]; })()""")
time.sleep(0.2)
if pos:
    page.send("Input.dispatchMouseEvent", {"type": "mouseMoved", "x": pos[0], "y": pos[1]})
    page.wait("document.querySelector('.hovercard.on')", "a hover card", 5)
    check(page.js("document.querySelector('.hovercard.on').textContent.length > 10"), f"hovering {pos[2]} shows its card")
    page.shot("hovercard")
    page.send("Input.dispatchMouseEvent", {"type": "mouseMoved", "x": 5, "y": 5})
collect("hover card")

# The inspector's back button.
page.goto(f"#/packages?i=pkg:{pick['pkg']}")
page.wait("document.querySelector('.insp-body .pkgsum .chip[data-ref^=\"pkg:\"]')", "the package summary")
nxt = page.js("(() => { const c = document.querySelector('.insp-body .pkgsum .chips .chip[data-ref^=\"pkg:\"]'); c.click(); return c.dataset.ref; })()")
page.wait(f"X.inspector.current() === {json.dumps(nxt)} && location.hash.indexOf('i=' + {json.dumps(nxt)}) > 0", "the inspector to follow the chip")
page.js("document.querySelector('.insp-head [data-a=\"back\"]').click()")
page.wait(f"X.inspector.current() === 'pkg:' + {json.dumps(pick['pkg'])}", "the inspector to go back", 5)
check(page.js(f"X.inspector.current() === 'pkg:' + {json.dumps(pick['pkg'])} && location.hash.indexOf('i=pkg:' + {json.dumps(pick['pkg'])}) > 0"), "the inspector's back button returns to the package, and the URL follows")
collect("inspector")

# A package page lists its files.
page.goto(f"#/pkg/{pick['pkg']}")
page.wait("document.querySelector('.pp-files tbody tr')", "the files table")
n = page.js("document.querySelectorAll('.pp-files tbody tr[data-key]').length")
check(n == pick["pkgFiles"], f"the page of {pick['pkg']} lists its {pick['pkgFiles']} files ({n})")
collect("package page")

# Stepping through a flow changes the step, the URL and the inspected code.
if pick["flow"]:
    page.goto(f"#/flows/{pick['flow']}?step=1")
    page.wait("document.querySelector('.fp-step .fs-nav') && X.inspector.current()", "the flow player and its first code")
    first = page.js("X.inspector.current()")
    page.js("document.dispatchEvent(new KeyboardEvent('keydown', {key: 'j', bubbles: true}))")
    page.wait("/[?&]step=2(&|$)/.test(location.hash)", "step 2 in the URL", 5)
    want = page.js(f"(() => {{ const e = X.model.stepAt(X.data.flow({json.dumps(pick['flow'])}), 2).step; return e.ref || ('actor:' + (e.at || e.from)); }})()")
    page.wait(f"X.inspector.current() === {json.dumps(want)}", "the inspector to follow the step", 5)
    cur = page.js("(document.querySelector('.seq-row.cur') || document.querySelector('.fl-step.cur') || {dataset: {}}).dataset.no")
    stepCount = page.js("document.querySelector('.fs-count').textContent")
    check(cur == "2" and "2" in stepCount and page.js(f"location.hash.indexOf('i=' + {json.dumps(want)}) > 0"),
          f"pressing j moves {pick['flow']} from step 1 ({first}) to step 2 ({want}): diagram, panel, URL and inspector follow")
    page.js("document.dispatchEvent(new KeyboardEvent('keydown', {key: 'k', bubbles: true}))")
    page.wait("/[?&]step=1(&|$)/.test(location.hash)", "back to step 1", 5)
    check(True, "pressing k goes back to step 1")
    collect("flow player")

# A reference in a concern's text opens the inspector.
if pick["concern"]:
    page.goto(f"#/concerns/{pick['concern']}")
    page.wait("document.querySelector('.cp-body')", "the concern page")
    ref = page.js("(() => { const a = [...document.querySelectorAll('.cp-body a.chip[data-ref]')].find(a => !/^diagram:/.test(a.dataset.ref));"
                  " if (!a) return null; a.scrollIntoView({block: 'center'}); a.click(); return a.dataset.ref; })()")
    if ref:
        page.wait(f"X.inspector.current() === {json.dumps(ref)} && document.querySelector('.insp-body').children.length", "the inspector to show the reference", 5)
        check(True, f"clicking {ref} in the text of {pick['concern']} opens it in the inspector")
    else:
        check(False, f"the text of {pick['concern']} has a reference to click")
    if pick["diagram"]:
        n = page.js("document.querySelectorAll('.cp-body .gd-fig').length")
        want = page.js(f"(X.data.concern({json.dumps(pick['concern'])}).diagrams || []).length")
        check(n == want, f"the page of {pick['concern']} shows its {want} diagrams ({n})")
    collect("concern")

# The runtime map draws a connector for every channel, and is fast.
if pick["actor"]:
    page.goto("#/overview")
    t0 = page.js("performance.now()")
    page.js("location.hash = '#/runtime'")
    page.wait("document.querySelectorAll('.rt-map g.cx').length > 0", "the runtime map's connectors")
    ms = int(page.js("performance.now()") - t0)
    n = page.js("document.querySelectorAll('.rt-map g.cx .cx-edge').length")
    routed = page.js("[...document.querySelectorAll('.rt-map g.cx .cx-edge')].filter(p => !/C/.test(p.getAttribute('d'))).length")
    check(n == pick["channels"], f"the runtime map draws {n} connectors for {pick['channels']} channels ({routed} routed through the gaps)")
    check(ms < 1500, f"the runtime map is drawn in {ms} ms")
    gates = page.js("document.querySelectorAll('.rt-map .cx-gate').length")
    want = page.js("X.data.model.channels.filter(c => c.boundary && c.from !== c.to && X.data.actor(c.from) && X.data.actor(c.to)).length")
    check(gates == want, f"every boundary channel carries a gate ({gates} of {want})")
    collect("runtime")

# A [[term:…]] shows its definition on hover.
if pick["term"]:
    page.goto(f"#/concerns/{pick['termConcern']}" if pick["termConcern"] else "#/glossary")
    page.wait("document.querySelector('[data-ref^=\"term:\"]')", "a term reference")
    pos = page.js("(() => { const a = document.querySelector('.cp-body [data-ref^=\"term:\"]') || document.querySelector('.gl-row [data-ref^=\"term:\"]');"
                  " if (!a) return null; a.scrollIntoView({block: 'center'}); const r = a.getBoundingClientRect(); return [r.left + Math.min(r.width / 2, 20), r.top + r.height / 2, a.dataset.ref]; })()")
    if pos:
        time.sleep(0.2)
        page.send("Input.dispatchMouseEvent", {"type": "mouseMoved", "x": pos[0], "y": pos[1]})
        page.wait("document.querySelector('.hovercard.on')", "the term's card", 5)
        words = page.js(f"X.md.plainText(X.data.term({json.dumps(pos[2][5:])}).text).split(/\\s+/).slice(0, 4).join(' ')")
        text = page.js("document.querySelector('.hovercard.on').textContent")
        check(words in " ".join(text.split()), f"hovering {pos[2]} shows its definition ({words!r}…)")
        page.shot("hovercard-term")
        page.send("Input.dispatchMouseEvent", {"type": "mouseMoved", "x": 5, "y": 5})
    else:
        check(False, "a term reference to hover")
    collect("term")

# The findings lead to the code that cites them.
if pick["finding"] and pick["findingCited"]:
    sel = f".fd-table tr[data-key=\"{pick['finding']}\"]"
    page.goto(f"#/docs/findings?q={pick['finding']}")
    page.wait(f"document.querySelector({json.dumps(sel + ' .chip[data-ref]')})", "the finding's citations")
    chips = page.js(f"document.querySelectorAll({json.dumps(sel + ' td:last-child .chip[data-ref]')}).length")
    check(chips > 0, f"the findings table lists where {pick['finding']} is cited ({chips} shown, {pick['findingCited']} in all)")
    page.js(f"document.querySelector({json.dumps(sel + ' a[data-row]')}).click()")
    page.wait("document.querySelector('.insp-body a.site')", "the finding in the inspector")
    site = page.js("(() => { const a = document.querySelector('.insp-body a.site'); a.click(); return [a.dataset.file, a.dataset.line]; })()")
    page.wait(f"location.hash.indexOf('#/code/' + {json.dumps(site[0])}) === 0 && /[?&]line={site[1]}(&|$)/.test(location.hash)", "the cited line", 5)
    check(True, f"a citation of {pick['finding']} opens {site[0]}:{site[1]}")
    collect("findings")

# The source marks a flow step, and the symbol panel says where the model
# mentions it.
if pick["stepFile"]:
    page.goto(f"#/code/{pick['stepFile']}")
    page.wait(f"document.querySelector('.src .r[data-n=\"{pick['stepLine']}\"] .mkx')", "the mark at the step's declaration")
    check(True, f"{pick['stepFile']}:{pick['stepLine']} carries a mark for a flow step")
    page.js(f"(() => {{ const s = X.data.symbolsOfFile({json.dumps(pick['stepFile'])}).find(x => x.line === {pick['stepLine']}); location.hash = '#/code/' + s.file + '?sym=' + s.ref; }})()")
    page.wait("document.querySelector('.cv-sym .bl-sec .bl')", "the symbol's backlinks")
    check(True, "its symbol panel lists where the model mentions it")
    collect("marks")

# Search finds a concern by its title.
if pick["concern"]:
    title = page.js(f"X.data.concern({json.dumps(pick['concern'])}).title")
    res = page.js(f"X.search.query({json.dumps(title)}).slice(0, 3).map(r => r.item.ref)")
    check("concern:" + pick["concern"] in res, f"search for {title!r} finds the concern ({res})")

# The largest document renders quickly.
if pick["doc"]:
    page.goto(f"#/docs/{pick['doc']}")
    page.wait("document.documentElement.dataset.docRenderMs", "the largest document")
    ms = int(page.js("document.documentElement.dataset.docRenderMs"))
    check(ms < 1000, f"the largest document ({pick['doc']}) renders in {ms} ms")
    collect("document")

# Every reference of the model, and one of every other kind, renders as a
# chip, a hover card and an inspector view without an exception.
page.goto("#/overview")
bad = page.js("""(() => {
  const c = X.data.core, refs = Object.keys(c.refs);
  refs.push('pkg:' + c.packages[0].path, 'file:' + c.files[0].path, 'cmd:' + c.commands[0].path, c.symbols[0].ref,
    'no/such.Thing', 'pkg:no/such', 'concern:no-such');
  ['rpc', 'routes', 'events', 'config', 'env'].forEach(k => { const x = c[k][0]; if (x) refs.push({rpc: 'rpc:', routes: 'http:', events: 'event:', config: 'config:', env: 'env:'}[k] + (x.name || x.key)); });
  const errs = [];
  refs.forEach(r => {
    try { X.refs.chip(r); X.refs.card(r); X.inspector.open(r); } catch (e) { errs.push(r + ': ' + e.message); }
  });
  X.inspector.close();
  return { n: refs.length, errs: errs.slice(0, 10) };
})()""")
time.sleep(1.5)
check(not bad["errs"], f"{bad['n']} references render as chips, cards and in the inspector" + (f": {bad['errs']}" if bad["errs"] else ""))
collect("references")

# The flows index: a filter narrows the flows, and the count follows.
page.goto("#/flows")
page.wait("document.querySelector('.fx-bar input') && document.querySelector('.fx-row')", "the flows index")
total = page.js("document.querySelectorAll('.fx-row').length")
word = page.js("(() => { const f = X.data.model.flows.find(f => f.kind !== 'tour'); return f ? f.title.split(/\\s+/).sort((a, b) => b.length - a.length)[0] : ''; })()")
page.js(f"(() => {{ const i = document.querySelector('.fx-bar input'); i.value = {json.dumps(word)}; i.dispatchEvent(new Event('input')); }})()")
time.sleep(0.4)
shown = page.js("document.querySelectorAll('.fx-row:not(.hidden)').length")
check(total == len(page.js("X.data.model.flows.map(f => f.id)")) and 0 < shown < total, f"the flows index lists {total} flows and tours; filtering by {word!r} leaves {shown}")
page.shot("flows-filtered")
collect("flows index")

# The glossary's index of terms: a click selects the term.
if pick["term"]:
    page.goto("#/glossary")
    page.wait("document.querySelector('.gl-index-in a[data-goto]')", "the glossary's index")
    tid = page.js("(() => { const as = document.querySelectorAll('.gl-index-in a[data-goto]'); const a = as[Math.floor(as.length / 2)]; a.click(); return a.dataset.goto; })()")
    page.wait(f"/[?&]sel={tid}(&|$)/.test(location.hash) && document.querySelector('.gl-t.sel[data-term=\"{tid}\"]')", "the term selected from the index", 5)
    check(page.js("document.querySelectorAll('.gl-index-in a[data-goto]').length") == page.js("X.data.model.terms.length"), f"the glossary's index lists every term, and a click selects {tid}")
    collect("glossary index")

# A concern page: "On this page" comes first and marks the section read.
if pick["concern"]:
    page.goto(f"#/concerns/{pick['concern']}")
    page.wait("document.querySelector('.cp-body')", "the concern page")
    first = page.js("(document.querySelector('.cp-side-in .cp-s') || {className: ''}).className")
    heads = page.js("document.querySelectorAll('.cp-body > h2').length")
    if heads > 2:
        page.js("(() => { const h = document.querySelectorAll('.cp-body > h2')[2]; h.scrollIntoView({block: 'start'}); })()")
        time.sleep(0.4)
        cur = page.js("(document.querySelector('.cp-toc a.cur') || {dataset: {}}).dataset.goto")
        want = page.js("document.querySelectorAll('.cp-body > h2')[2].dataset.anchor")
        check("toc" in first and cur == want, f"the side column of {pick['concern']} starts with \"On this page\", which marks the section in view ({cur})")
    # A table wider than the text scrolls sideways with a cue.
    page.js("""(() => { const d = document.createElement('div'); d.className = 'md'; d.style.width = '300px'; d.id = 'wide-test';
      d.innerHTML = X.md.render('| a | b | c | d |\\n|---|---|---|---|\\n| `internal/daemon.requestService.decideLocked` | `internal/hub.Server.apiDecide` | `POST /api/requests/{machine}/{id}/decide` | x |', {});
      document.querySelector('.cp-body').prepend(d); return 1; })()""")
    time.sleep(0.4)
    check(page.js("document.querySelector('#wide-test .md-tw').classList.contains('wide')") and page.js("getComputedStyle(document.querySelector('#wide-test .md-tw'), '::before').content.indexOf('scroll') >= 0"),
          "a table wider than its column is marked and says it scrolls sideways")
    collect("concern side column")

# Diagrams: every label of a loop is drawn, inside its diagram.
loops = page.js("""(() => { const out = []; (X.data.model.concerns || []).forEach(c => (c.diagrams || []).forEach(d => (d.edges || []).forEach(e => { if (e.from === e.to && e.label) out.push([c.id, d.id, e.label]); }))); return out; })()""")
for cid, did, lab in loops[:4]:
    page.goto(f"#/concerns/{cid}?diagram={did}")
    page.wait(f"document.querySelector('.gd-fig[data-diagram=\"{cid}/{did}\"] .cx-lab')", f"the diagram {did}")
    ok = page.js(f"""(() => {{ const fig = document.querySelector('.gd-fig[data-diagram="{cid}/{did}"]'), sc = fig.querySelector('.gd-scroll').getBoundingClientRect();
      const l = [...fig.querySelectorAll('.cx-lab')].find(x => x.textContent === {json.dumps(lab)}); if (!l) return false;
      const r = l.getBoundingClientRect(); return r.width > 0 && r.top >= sc.top && r.bottom <= sc.bottom && r.left >= sc.left - 1; }})()""")
    check(ok, f"the loop {lab!r} of {cid}/{did} shows its label inside the diagram")
    collect("loops")

# Flows: ?step=N shows the step in the diagram; a tour step that names only
# code shows the name once; an unchecked reason is shown; a path through a
# function value is marked weaker; source margins have one mark per kind.
if pick["flow"]:
    n = pick["flowSteps"]
    page.goto(f"#/flows/{pick['flow']}?step={n}")
    page.wait("document.querySelector('.seq-row.cur') && X.inspector.current()", "the last step")
    time.sleep(0.6)
    vis = page.js("(() => { const d = document.querySelector('.fp-diagram').getBoundingClientRect(), r = document.querySelector('.seq-row.cur').getBoundingClientRect(); return r.top >= d.top && r.bottom <= d.bottom; })()")
    check(vis, f"opening {pick['flow']} at step {n} shows that step in the diagram")
    collect("step in view")
if pick["tour"]:
    page.goto(f"#/flows/{pick['tour']}")
    page.wait("document.querySelector('.fl-step')", "the tour's steps")
    dup = page.js("[...document.querySelectorAll('.fl-step')].filter(li => { const w = li.querySelector('.fl-who'), t = li.querySelector('.fl-what'); return w && t && w.textContent.trim() && w.textContent.trim() === t.textContent.trim(); }).length")
    check(dup == 0, f"no step of {pick['tour']} shows its name twice")
    collect("tour")
unc = page.js("(() => { for (const f of X.data.model.flows) { if (f.kind === 'tour') continue; const st = X.model.steps(f); for (const e of st) if (e.step.unchecked) return [f.id, e.no]; } return null; })()")
if unc:
    page.goto(f"#/flows/{unc[0]}?step={unc[1]}")
    page.wait("document.querySelector('.fp-step .fs-nav')", "a step with an unchecked reason")
    check(page.js("document.querySelector('.fp-step').textContent.indexOf('The tool cannot see this connection') >= 0"), f"{unc[0]} step {unc[1]} shows its reason as not verified")
    collect("unchecked")
weak = page.js("(() => { for (const f of X.data.model.flows) { const st = X.model.steps(f); for (const e of st) if ((e.step.connections || []).some(c => (c.valueEdges || []).length)) return [f.id, e.no]; } return null; })()")
if weak:
    page.goto(f"#/flows/{weak[0]}?step={weak[1]}")
    page.wait("document.querySelector('.fp-step .fs-nav')", "a step reached through a function value")
    check(page.js("!!document.querySelector('.fp-step .fs-hop.val') && !!document.querySelector('.fp-step .tag.weak')"), f"{weak[0]} step {weak[1]} marks the path through a function value as weaker")
    collect("weak path")
if pick["stepFile"]:
    page.goto(f"#/code/{pick['stepFile']}")
    page.wait("document.querySelector('.src .mk .mkx')", "the margin marks")
    dupk = page.js("[...document.querySelectorAll('.src .mk')].filter(m => { const k = [...m.children].map(x => x.className); return new Set(k).size !== k.length; }).length")
    check(dupk == 0, "no line of the source carries two marks of one kind")
    collect("marks per kind")

# The runtime map, filtered by a concern, names the channels it keeps.
fc = page.js("(() => { const c = (X.data.model.concerns || []).find(c => (X.data.model.channels || []).some(ch => (ch.concerns || []).indexOf(c.id) >= 0)); return c ? c.id : ''; })()")
if fc:
    page.goto(f"#/runtime?concern={fc}")
    page.wait("document.querySelector('.rt-map.filtered g.cx')", "the filtered map")
    time.sleep(0.5)
    check(page.js("document.querySelectorAll('.rt-map .cx-lab.on').length > 0"), f"the runtime map filtered by {fc} labels the channels it keeps")
    collect("filtered map labels")

# Speed: the first paint, and opening the largest file.
page.goto("#/overview")
t = page.js("({ started: Math.round(X.startedAt || 0), fcp: Math.round((performance.getEntriesByName('first-contentful-paint')[0] || {}).startTime || 0) })")
check(t["started"] < 1500, f"the app starts in {t['started']} ms (first contentful paint {t['fcp']} ms)")
page.goto(f"#/code/{pick['big']}")
page.wait("document.documentElement.dataset.fileOpenMs", "the largest file")
ms = int(page.js("document.documentElement.dataset.fileOpenMs"))
check(ms < 1500, f"the largest file ({pick['big']}, {pick['bigLines']} lines) opens in {ms} ms")
collect("speed")

print(f"{count} screens checked; screenshots in {out}")
if problems:
    print("problems:")
    for p in problems:
        print("  " + p)
if problems or failures:
    sys.exit(1)
print("no errors or exceptions; all assertions hold")
PY

python3 -I "$WORK/cdp.py" "$SITE" "$CDP_PORT" "$OUT" "$FULL" ${SHOTS[@]+"${SHOTS[@]}"}
