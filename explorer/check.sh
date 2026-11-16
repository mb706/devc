#!/usr/bin/env bash
# Checks the explorer app in a headless Chrome (work order X2): builds the
# site, opens it from its file:// URL, visits every route of the app's
# views in light and dark at 1280 pixels (and a few at 1920), saves a
# screenshot of each, fails on any console error or uncaught exception,
# and runs assertions in the page: search finds a symbol, following a link
# in the source lands on the definition, the inspector's back button
# works, a package page lists its files, and the largest file opens in
# about a second. Nothing about the code base is written here: the routes
# and the things to look for are chosen from the site's data.
#
#   explorer/check.sh [--no-build] [output-directory]
#
# Needs python3 (standard library only: a minimal DevTools protocol client
# below) and Chrome or Chromium: $CHROME, else chrome-headless-shell or
# chrome under ~/.local/opt, else chromium or google-chrome on the PATH.
# Everything it starts is stopped on exit.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
BUILD=1
OUT_ARG=""
while [ "$#" -gt 0 ]; do
	case "$1" in
	--no-build) BUILD=0; shift ;;
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
SITE="$REPO/explorer/dist/index.html"
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

site, cdp, out = sys.argv[1], sys.argv[2], sys.argv[3]
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
  return {
    big: big.path, bigLines: lines(big), other: other ? other.path : '', pkg: pkg.path, pkgFiles: pkg.files.length,
    used: used.ref, usedName: used.name, fn: fn.ref, iface: iface ? iface.ref : '', cmd: cmd.path, cfg: cfg ? cfg.key : '',
    rpc: c.rpc.length ? c.rpc[0].name : '', dir: pkg.path.split('/')[0],
    surfaces: (X.data.model.surfaces || []).map(s => s.id)
  };
})()""")

routes = [
    ("overview", "#/overview", ".stats"),
    ("runtime", "#/runtime", ".placeholder"),
    ("concerns", "#/concerns", ".placeholder"),
    ("flows", "#/flows", ".placeholder"),
    ("state", "#/state", ".placeholder"),
    ("glossary", "#/glossary", ".placeholder"),
    ("docs", "#/docs", ".placeholder"),
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
if pick["iface"]:
    routes.append(("code-interface", f"#/code/?sym={pick['iface']}", ".cv-sym .sp-dyn"))
if pick["rpc"]:
    routes.append(("insp-rpc", f"#/packages?i=rpc:{pick['rpc']}", ".insp-body .sp-sec"))
for s in pick["surfaces"]:
    routes.append((f"interfaces-{s}", f"#/interfaces/{s}", ".xt-table tbody tr"))

wide = {"packages", "code-big", "code-symbol", "insp-symbol", "package", "size", "interfaces-" + (pick["surfaces"][0] if pick["surfaces"] else "")}
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

python3 -I "$WORK/cdp.py" "$SITE" "$CDP_PORT" "$OUT"
