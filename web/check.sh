#!/usr/bin/env bash
# Checks the web app in a headless Chrome (work order C12 section 5): starts
# the hub's development mode (`devc hub serve --dev-no-auth`, invented
# machines, loopback only), opens every screen at 390x844 (touch) and
# 1280x800 in light and dark, saves a screenshot and the rendered DOM of
# each, tries a few actions (answer a question, send a prompt, open the
# terminal, expand a tool run, page back through a long history), checks
# logic of the app's modules in the page, and fails when a page logs an
# error, throws, or violates the content security policy.
#
#   web/check.sh [output-directory]
#   web/check.sh --url http://127.0.0.1:PORT --session MACHINE/SESSION [output-directory]
#
# The second form checks a hub that runs already (the integration test
# TestWebApp puts a real hub with a real machine behind a loopback proxy
# that adds the device certificate and the login): the inbox, the named
# session's timeline, terminal and changes, projects and machines; it
# answers the session's pending question and sends it a prompt.
#
# Needs bin/devc (./dev build) for the first form, python3 (standard library
# only: a minimal DevTools protocol client below) and Chrome or Chromium:
# $CHROME, else chrome-headless-shell or chrome under ~/.local/opt, else
# chromium or google-chrome on the PATH. Everything it starts is stopped on
# exit.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
URL=""
SESSION=""
OUT_ARG=""
while [ "$#" -gt 0 ]; do
	case "$1" in
	--url) URL="$2"; shift 2 ;;
	--session) SESSION="$2"; shift 2 ;;
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
	echo "web/check.sh: no Chrome or Chromium found (set CHROME)" >&2
	exit 1
}
CHROME_BIN="$(find_chrome)"
[ -n "$URL" ] || [ -x "$REPO/bin/devc" ] || { echo "web/check.sh: build bin/devc first (./dev build)" >&2; exit 1; }

WORK="$(mktemp -d)"
HUB_PID=""
CHROME_PID=""
cleanup() {
	[ -n "$CHROME_PID" ] && kill "$CHROME_PID" 2>/dev/null && wait "$CHROME_PID" 2>/dev/null || true
	[ -n "$HUB_PID" ] && kill "$HUB_PID" 2>/dev/null && wait "$HUB_PID" 2>/dev/null || true
	rm -rf "${WORK:?}"
}
trap cleanup EXIT

free_port() { python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
HUB_PORT="$(free_port)"
CDP_PORT="$(free_port)"

if [ -z "$URL" ]; then
	DEVC_DEV=1 "$REPO/bin/devc" hub serve --dev-no-auth --listen "127.0.0.1:$HUB_PORT" >"$OUT/hub.log" 2>&1 &
	HUB_PID=$!
	URL="http://127.0.0.1:$HUB_PORT"
fi
"$CHROME_BIN" --headless --no-sandbox --disable-gpu --hide-scrollbars --no-first-run --no-default-browser-check \
	--user-data-dir="$WORK/profile" --remote-debugging-address=127.0.0.1 --remote-debugging-port="$CDP_PORT" \
	about:blank >"$OUT/chrome.log" 2>&1 &
CHROME_PID=$!

cat >"$WORK/cdp.py" <<'PY'
import base64, json, os, socket, struct, sys, time, urllib.request

base, cdp, out = sys.argv[1].rstrip("/"), sys.argv[2], sys.argv[3]
real = sys.argv[4] if len(sys.argv) > 4 else ""

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
    def send(self, method, params=None, timeout=30):
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
            raise SystemExit(f"evaluating {expr!r}: {r['exceptionDetails']}")
        return r["result"].get("value")
    def wait(self, expr, what, timeout=15):
        end = time.time() + timeout
        while time.time() < end:
            r = self.send("Runtime.evaluate", {"expression": f"(() => {{ try {{ return !!({expr}); }} catch (e) {{ return false; }} }})()", "returnByValue": True})
            if r.get("result", {}).get("value"):
                return
            time.sleep(0.2)
        raise SystemExit(f"timed out waiting for {what}")
    def wait_event(self, method, timeout=10):
        end = time.time() + timeout
        while time.time() < end:
            for m in self.events:
                if m.get("method") == method:
                    self.events.remove(m)
                    return m
            self.events.append(self._recv())
        raise SystemExit(f"no {method} event")
    def goto(self, url):
        # Through about:blank, so that a change of the hash alone is a new
        # page load too.
        self.send("Page.navigate", {"url": "about:blank"})
        self.wait("location.href === 'about:blank'", "about:blank")
        self.send("Page.navigate", {"url": url})

wait_http(base + "/")
wait_http(f"http://127.0.0.1:{cdp}/json/version")
if real:
    m, s = real.split("/")
    question = working = {"machine": m, "session": s}
else:
    time.sleep(1)  # the invented machines connect
    inbox = json.loads(wait_http(base + "/api/inbox"))
    sessions = [s for sec in inbox["sections"] for s in sec["sessions"]]
    question = next(s for s in sessions if s.get("question"))
    working = next(s for s in sessions if s["machine"] == "desktop" and s["state"] == "working")
    history = next(s for s in sessions if s["machine"] == "desktop" and s.get("worktree") == "history")

req = urllib.request.Request(f"http://127.0.0.1:{cdp}/json/new?about:blank", method="PUT")
target = json.loads(urllib.request.urlopen(req).read())
page = Page(target["webSocketDebuggerUrl"])
for d in ("Page", "Runtime", "Log"):
    page.send(d + ".enable")
# A headless page has no window focus; text input needs a focused element.
page.send("Emulation.setFocusEmulationEnabled", {"enabled": True})

def sid(s, tab=""):
    return f"#/s/{s['machine']}/{s['session']}" + (f"/{tab}" if tab else "")

screens = [
    ("inbox", "#/", "document.querySelectorAll('.section .row').length > 5"),
    ("session", sid(question), "document.querySelector('.qcard') && document.querySelectorAll('.timeline .tl').length > 3"),
    ("terminal", sid(working, "terminal"), "(document.querySelector('.xterm-rows') || {}).textContent?.includes('Welcome')"),
    ("changes", sid(question, "changes"), "document.querySelectorAll('.files li').length > 1 && document.querySelector('pre.diff')"),
    ("projects", "#/projects", "document.querySelectorAll('.section .row').length > 2"),
    ("project", "#/p/laptop/work/api", "document.querySelectorAll('.wts li').length > 1"),
    ("new", "#/new", "document.querySelectorAll('form.form select').length >= 4"),
    ("machines", "#/machines", "document.querySelectorAll('.row').length >= 3"),
    ("login", "#/login", "document.querySelector('form.card input[type=password]')"),
]
sizes = [("phone", 390, 844, True), ("desktop", 1280, 800, False)]
if real:
    screens = [
        ("inbox", "#/", "document.querySelectorAll('.section .row').length >= 1"),
        ("session", sid(question), "document.querySelectorAll('.timeline .tl').length >= 2"),
        ("terminal", sid(working, "terminal"), "(document.querySelector('.xterm-rows') || {}).textContent?.trim().length > 0 && !document.querySelector('.term-msg')"),
        ("changes", sid(question, "changes"), "document.querySelector('.files li, .changes .note')"),
        ("projects", "#/projects", "document.querySelectorAll('.section .row').length >= 1"),
        ("machines", "#/machines", "document.querySelectorAll('.row').length >= 1"),
    ]
problems = []

def collect(label):
    for m in page.events:
        meth, p = m.get("method"), m.get("params", {})
        if meth == "Runtime.exceptionThrown":
            problems.append(f"{label}: exception: {p['exceptionDetails'].get('exception', {}).get('description') or p['exceptionDetails'].get('text')}")
        elif meth == "Runtime.consoleAPICalled" and p.get("type") in ("error", "assert"):
            problems.append(f"{label}: console.{p['type']}: " + " ".join(str(a.get('value', a.get('description', ''))) for a in p.get("args", [])))
        elif meth == "Log.entryAdded" and p["entry"].get("level") == "error" and "sourceMappingURL" not in p["entry"].get("text", "") and ".map" not in p["entry"].get("url", ""):
            problems.append(f"{label}: {p['entry'].get('source')}: {p['entry'].get('text')} {p['entry'].get('url', '')}")
    page.events = []

# The check must be able to see a policy violation: provoke one.
page.goto(f"{base}/#/")
page.wait("document.documentElement.dataset.app === 'started'", "the app to start")
page.js("document.body.insertAdjacentHTML('beforeend', '<i style=\"color: red\"></i>')")
time.sleep(0.5)
page.js("1")
collect("self-test")
if not any("Content Security Policy" in p for p in problems):
    raise SystemExit("the check did not notice a deliberate policy violation: " + repr(problems))
problems.clear()

for size, w, h, mobile in sizes:
    page.send("Emulation.setDeviceMetricsOverride", {"width": w, "height": h, "deviceScaleFactor": 1, "mobile": mobile})
    page.send("Emulation.setTouchEmulationEnabled", {"enabled": mobile})
    for scheme in ("light", "dark"):
        page.send("Emulation.setEmulatedMedia", {"features": [{"name": "prefers-color-scheme", "value": scheme}]})
        for name, frag, ready in screens:
            label = f"{name}-{size}-{scheme}"
            page.goto(f"{base}/{frag}")
            page.wait("document.documentElement.dataset.app === 'started'", f"{label}: the app to start")
            page.wait(ready, f"{label}: {ready}")
            page.wait("document.querySelector('.conn-live') || !document.querySelector('.conn')", f"{label}: the event stream", 10)
            time.sleep(0.6)
            shot = page.send("Page.captureScreenshot", {"format": "png"})
            open(os.path.join(out, label + ".png"), "wb").write(base64.b64decode(shot["data"]))
            open(os.path.join(out, label + ".html"), "w").write(page.js("document.documentElement.outerHTML"))
            collect(label)

if real:
    # On the real session: answer its question when one is pending, send a
    # prompt, and see the reply in the timeline and on the terminal.
    page.send("Emulation.setDeviceMetricsOverride", {"width": 390, "height": 844, "deviceScaleFactor": 1, "mobile": True})
    page.send("Emulation.setEmulatedMedia", {"features": [{"name": "prefers-color-scheme", "value": "light"}]})
    page.goto(f"{base}/{sid(question)}")
    page.wait("document.querySelectorAll('.timeline .tl').length >= 2", "the timeline")
    if page.js("!!document.querySelector('.qcard .btn.opt')"):
        open(os.path.join(out, "action-question.png"), "wb").write(base64.b64decode(page.send("Page.captureScreenshot", {"format": "png"})["data"]))
        page.js("document.querySelector('.qcard .btn.opt').click()")
        page.wait("!document.querySelector('.qcard') && document.body.textContent.includes('Answered with option 1')", "the answer in the timeline", 30)
        page.wait("!!document.querySelector('.composer textarea')", "the composer", 30)
    page.js("""(() => { const t = document.querySelector('.composer textarea');
      t.value = 'say from the browser'; t.dispatchEvent(new Event('input', {bubbles: true})); })()""")
    page.js("document.querySelector('.composer button[type=submit]').click()")
    page.wait("[...document.querySelectorAll('.tl-message .msg')].some(m => m.textContent.trim() === 'from the browser')", "the reply in the timeline", 60)
    open(os.path.join(out, "action-session.png"), "wb").write(base64.b64decode(page.send("Page.captureScreenshot", {"format": "png"})["data"]))
    page.goto(f"{base}/{sid(working, 'terminal')}")
    page.wait("(document.querySelector('.xterm-rows') || {}).textContent?.includes('from the browser')", "the reply on the terminal", 30)
    open(os.path.join(out, "action-terminal.png"), "wb").write(base64.b64decode(page.send("Page.captureScreenshot", {"format": "png"})["data"]))
    collect("actions")
    print(f"{len(screens) * 4} screens and the session's actions checked; screenshots and DOM in {out}")
    if problems:
        print("problems:")
        for p in problems:
            print("  " + p)
        sys.exit(1)
    print("no errors, exceptions or policy violations")
    sys.exit(0)

# Actions, at phone size in light: they go through the API to an invented
# machine and back through the event stream.
page.send("Emulation.setDeviceMetricsOverride", {"width": 390, "height": 844, "deviceScaleFactor": 1, "mobile": True})
page.send("Emulation.setEmulatedMedia", {"features": [{"name": "prefers-color-scheme", "value": "light"}]})
page.goto(f"{base}/{sid(question)}")
page.wait("document.querySelector('.qcard .btn.opt')", "the question card")
page.js("document.querySelector('.toolrun').click()")
page.wait("document.querySelector('.toollist li')", "an expanded tool run")
page.js("document.querySelector('.qcard .btn.opt').click()")
page.wait("!document.querySelector('.qcard') && document.body.textContent.includes('Answered with option 1')", "the answer in the timeline")
page.wait("document.body.textContent.includes('Done: migration written for')", "the turn after the answer", 20)
page.js("""(() => { const t = document.querySelector('.composer textarea');
  t.value = 'Also add an index on last_seq.'; t.dispatchEvent(new Event('input', {bubbles: true})); })()""")
page.js("document.querySelector('.composer button[type=submit]').click()")
page.wait("[...document.querySelectorAll('.tl-prompt .bubble')].some(b => b.textContent.includes('index on last_seq') && !b.closest('.pending'))", "the sent prompt in the timeline", 20)
open(os.path.join(out, "action-session.png"), "wb").write(base64.b64decode(page.send("Page.captureScreenshot", {"format": "png"})["data"]))
collect("actions: session")

# A prompt whose outcome is unknown: every attempt to send it fails on the
# network. It stays in the timeline as unknown, the receipt the app then
# asks for says it never arrived, and "Send again" delivers it with its
# own ID.
page.send("Fetch.enable", {"patterns": [{"urlPattern": "*/api/commands", "requestStage": "Request"}]})
page.js("""(() => { const t = document.querySelector('.composer textarea');
  t.value = 'A prompt the network loses.'; t.dispatchEvent(new Event('input', {bubbles: true})); })()""")
page.js("document.querySelector('.composer button[type=submit]').click()")
lost = 0
while lost < 4:  # the app's attempts with the prompt's ID; other commands pass
    paused = page.wait_event("Fetch.requestPaused", 15)["params"]
    if "network loses" in paused["request"].get("postData", ""):
        page.send("Fetch.failRequest", {"requestId": paused["requestId"], "errorReason": "ConnectionReset"})
        lost += 1
    else:
        page.send("Fetch.continueRequest", {"requestId": paused["requestId"]})
page.send("Fetch.disable")
page.wait("[...document.querySelectorAll('.tl-prompt.uncertain .bubble')].some(b => b.textContent.includes('network loses'))", "the prompt shown as unknown", 15)
page.wait("[...document.querySelectorAll('.tl-prompt.uncertain .pend-actions button')].some(b => b.textContent === 'Send again')", "the receipt saying it never arrived", 25)
page.js("document.querySelector('.tl-prompt.uncertain').scrollIntoView({block: 'center'})")
time.sleep(0.3)
open(os.path.join(out, "action-unconfirmed.png"), "wb").write(base64.b64decode(page.send("Page.captureScreenshot", {"format": "png"})["data"]))
page.js("[...document.querySelectorAll('.tl-prompt.uncertain .pend-actions button')].find(b => b.textContent === 'Send again').click()")
page.wait("[...document.querySelectorAll('.tl-prompt .bubble')].some(b => b.textContent.includes('network loses') && !b.closest('.pending'))", "the prompt delivered by Send again", 20)
# The network errors of the lost attempts are expected; nothing else may be logged.
page.events = [m for m in page.events if not (m.get("method") == "Log.entryAdded" and "/api/commands" in m.get("params", {}).get("entry", {}).get("url", ""))]
collect("actions: unconfirmed")

# A long history (findings D1-09, D1-13): tool runs stay bounded, "Load
# earlier" moves the bounded window back to the session's start, and "Back
# to the latest" returns to the live end.
page.goto(f"{base}/{sid(history)}")
page.wait("document.body.textContent.includes('Turn 12: the posts of 2024')", "the long session's latest turn", 20)
store_k = f"{history['machine']}/{history['session']}"
runs = page.js("[...document.querySelectorAll('.tr-n')].map(e => parseInt(e.textContent))")
if not runs or max(runs) > 200:
    raise SystemExit(f"tool runs of {runs} calls")
page.js("[...document.querySelectorAll('.toolrun')].pop().click()")
page.wait("document.querySelector('.toollist li')", "an expanded long tool run")
if page.js("document.querySelectorAll('.toollist li').length") > 200:
    raise SystemExit("an expanded tool run draws more than 200 rows")
for _ in range(40):
    if page.js("document.body.textContent.includes('Started claude in history')"):
        break
    page.wait("[...document.querySelectorAll('.tl-more button')].some(b => !b.disabled && /Load earlier|Show earlier/.test(b.textContent))", "a control for earlier events", 20)
    page.js("[...document.querySelectorAll('.tl-more button')].find(b => !b.disabled && /Load earlier|Show earlier/.test(b.textContent)).click()")
    time.sleep(0.3)
else:
    raise SystemExit("the window never reached the session's start")
held = page.js(f"import('/app/store.js').then(m => m.state.details[{json.dumps(store_k)}].events.length)")
if held > 1000:
    raise SystemExit(f"the window holds {held} events")
page.wait("[...document.querySelectorAll('.tl-latest button')].some(b => b.textContent.includes('Back to the latest'))", "the control back to the latest events")
page.js("document.querySelector('.tl-top-marker').scrollIntoView()")
time.sleep(0.3)
open(os.path.join(out, "action-history.png"), "wb").write(base64.b64decode(page.send("Page.captureScreenshot", {"format": "png"})["data"]))
page.js("[...document.querySelectorAll('.tl-latest button')].find(b => b.textContent.includes('Back to the latest')).click()")
page.wait("!document.querySelector('.tl-latest') && document.body.textContent.includes('Turn 12: the posts of 2024')", "the latest events again", 20)
collect("actions: history")

# Logic the screens cannot show directly, in the page's own modules.
checks = page.js("""(async () => {
  const out = [];
  const tl = await import('/app/timeline.js');
  const api = await import('/app/api.js');
  const store = await import('/app/store.js');
  // A tool run that never ends stays bounded, also its first run after a
  // trim (D1-09).
  const d = tl.newDetail();
  for (let i = 1; i <= 3000; i++) tl.append(d, { seq: i, type: 'tool', data: { name: 'Read', summary: 'f' + i } });
  const sizes = d.items.map((it) => it.list.length);
  if (d.events.length !== 1000 || Math.max(...sizes) > tl.TOOL_RUN_MAX || d.items[0].list[0].seq < d.events[0].seq) out.push('tool runs: ' + JSON.stringify(sizes));
  // Paging back beyond the window moves it and leaves the live end (D1-13).
  const older = []; for (let i = 1801; i <= 2000; i++) older.push({ seq: i, type: 'note', data: { text: 'n' } });
  tl.prepend(d, older);
  if (d.events.length !== 1000 || d.events[0].seq !== 1801 || d.live) out.push('prepend: ' + d.events.length + ' ' + d.events[0].seq + ' ' + d.live);
  const realFetch = window.fetch;
  try {
    // A successful answer that breaks off is an uncertain outcome (D1-10).
    window.fetch = async () => new Response('{"command":"x","outc', { status: 200, headers: { 'Content-Type': 'application/json' } });
    try { await api.get('/api/me'); out.push('a broken answer was read'); } catch (e) { if (!e.uncertain) out.push('a broken answer is not uncertain'); }
    const ids = [];
    window.fetch = async (url, opts) => { if (opts && opts.body) ids.push(JSON.parse(opts.body).id); return new Response('{"command":', { status: 200 }); };
    const rec = await store.run('nomachine', 'interrupt', { session: 's0000000000000000000z' }, { quiet: true, id: 'check-id-1' });
    const p = store.state.pending['check-id-1'];
    if (!rec || rec.outcome !== 'unknown' || !p || p.id !== 'check-id-1' || ids.length < 2 || ids.some((i) => i !== 'check-id-1')) out.push('uncertain command: ' + JSON.stringify({ rec, p: !!p, ids }));
    store.dismissPending('check-id-1');
    // A detail read that began before a newer summary does not undo it (D1-12).
    const k = 'nomachine/s0000000000000000000z';
    store.applyChange('session', { machine: 'nomachine', session: 's0000000000000000000z', question: { id: 'q2' }, last_seq: 9 }, 1000000);
    window.fetch = async () => new Response(JSON.stringify({ session: { machine: 'nomachine', session: 's0000000000000000000z', question: { id: 'q1' }, last_seq: 8 }, events: [], more: false, stream_id: 999999 }), { status: 200 });
    await store.loadDetail('nomachine', 's0000000000000000000z');
    if (!store.state.sessions[k] || store.state.sessions[k].question.id !== 'q2') out.push('an older summary replaced a newer one');
    store.dropDetail('nomachine', 's0000000000000000000z');
    store.applyChange('session.removed', { machine: 'nomachine', session: 's0000000000000000000z' });

    // Uncertainty is kept (E1-11): the first attempt's answer breaks off,
    // the retry is refused outright (the machine went offline); the command
    // stays pending under its ID. A retry of a pending command that is
    // refused outright stays pending too; one the machine never received
    // ('unsent') fails.
    const ids2 = [];
    let n = 0;
    window.fetch = async (url, opts) => {
      if (opts && opts.body) ids2.push(JSON.parse(opts.body).id);
      n++;
      if (n === 1) return new Response('{"command":', { status: 200 });
      return new Response('{"error":"machine nomachine is offline"}', { status: 503 });
    };
    const rec2 = await store.run('nomachine', 'interrupt', { session: 's0000000000000000000z' }, { quiet: true, id: 'check-id-2' });
    if (!rec2 || rec2.outcome !== 'unknown' || !store.state.pending['check-id-2'] || ids2.length !== 2 || ids2.some((i) => i !== 'check-id-2')) out.push('uncertainty lost after a refusal: ' + JSON.stringify({ rec2, ids2 }));
    window.fetch = async () => new Response('{"error":"machine nomachine is offline"}', { status: 503 });
    await store.retryPending('check-id-2');
    if (!store.state.pending['check-id-2']) out.push('a refused retry of an uncertain command dropped it');
    else {
      store.state.pending['check-id-2'].outcome = 'unsent';
      await store.retryPending('check-id-2');
      if (store.state.pending['check-id-2']) out.push('a refused retry of an unsent command is still pending');
      store.dismissPending('check-id-2');
    }

    // Paging back while live events keep coming (E1-13): the page is
    // applied, the live events are counted, not appended.
    const m3 = 'nomachine', s3 = 's0000000000000000000y', k3 = m3 + '/' + s3;
    const evs = (a, b) => { const r = []; for (let i = a; i <= b; i++) r.push({ seq: i, type: 'note', data: { text: 'e' + i } }); return r; };
    const d3 = tl.newDetail();
    store.state.details[k3] = d3;
    d3.buffer = []; tl.replace(d3, evs(2001, 3000)); d3.more = true;
    let release;
    window.fetch = (url) => new Promise((resolve) => { release = () => resolve(new Response(JSON.stringify({ session: { machine: m3, session: s3 }, events: evs(1801, 2000), more: true, stream_id: 1 }), { status: 200 })); });
    const older2 = store.loadDetail(m3, s3, 2001);
    for (let i = 3001; i <= 3050; i++) store.applyChange('event', { machine: m3, session: s3, event: { seq: i, type: 'note', data: { text: 'live' } } }, 0);
    release();
    await older2;
    if (d3.events[0].seq !== 1801 || d3.events.length !== 1000 || d3.live || tl.newer(d3) !== 250) out.push('paging back under live events: ' + JSON.stringify({ first: d3.events[0].seq, n: d3.events.length, live: d3.live, newer: tl.newer(d3) }));
    // Back to the latest (E1-14): an event that arrives while the latest
    // events are read is merged.
    window.fetch = (url) => new Promise((resolve) => { release = () => resolve(new Response(JSON.stringify({ session: { machine: m3, session: s3 }, events: evs(2751, 3050), more: true, stream_id: 2 }), { status: 200 })); });
    const latest2 = store.loadDetail(m3, s3);
    store.applyChange('event', { machine: m3, session: s3, event: { seq: 3051, type: 'note', data: { text: 'meanwhile' } } }, 0);
    release();
    await latest2;
    if (!d3.live || tl.lastSeq(d3) !== 3051 || d3.loading) out.push('back to the latest lost an event: ' + JSON.stringify({ live: d3.live, last: tl.lastSeq(d3) }));
    store.dropDetail(m3, s3);
    store.applyChange('session.removed', { machine: m3, session: s3 });

    // A prompt event from the box never settles a waiting prompt (X12 6):
    // a short or matching event is only noted; the outbox entry and the
    // pending command stay until a receipt, which then settles them.
    const m4 = 'nomachine', s4 = 's0000000000000000000x', k4 = m4 + '/' + s4;
    store.state.details[k4] = tl.newDetail();
    window.fetch = async () => new Response('{"command":', { status: 200 });
    const rec4 = await store.sendPrompt({ machine: m4, session: s4, ref: 'demo/hello' }, 'yes, go ahead', 'queue');
    const id4 = rec4 && rec4.command;
    const entry4 = () => (store.state.outbox[k4] || []).find((i) => i.id === id4);
    for (const [seq, text] of [[1, 'y'], [2, '   ']]) store.applyChange('event', { machine: m4, session: s4, event: { seq, type: 'prompt', data: { text } } }, 0);
    if (!id4 || !store.state.pending[id4] || !entry4() || entry4().seen) out.push('a short prompt event settled a waiting prompt: ' + JSON.stringify({ id4, pending: !!store.state.pending[id4], entry: entry4() }));
    store.applyChange('event', { machine: m4, session: s4, event: { seq: 3, type: 'prompt', data: { text: 'yes, go ahead' } } }, 0);
    if (!store.state.pending[id4] || !entry4() || !entry4().seen) out.push('a matching prompt event settled a waiting prompt: ' + JSON.stringify({ pending: !!store.state.pending[id4], entry: entry4() }));
    window.fetch = async () => new Response(JSON.stringify({ command: id4, outcome: 'delivered', final: true }), { status: 200 });
    await store.retryPending(id4);
    if (store.state.pending[id4] || !entry4() || entry4().outcome !== 'delivered') out.push('the final receipt did not settle the prompt: ' + JSON.stringify({ pending: !!store.state.pending[id4], entry: entry4() }));
    store.dropDetail(m4, s4);
    store.applyChange('session.removed', { machine: m4, session: s4 });
  } finally {
    window.fetch = realFetch;
  }
  return out;
})()""")
if checks:
    raise SystemExit("logic checks: " + "; ".join(checks))
# The broken answers above are deliberate.
page.events = [m for m in page.events if not (m.get("method") == "Log.entryAdded")]
collect("logic checks")

page.goto(f"{base}/{sid(working, 'terminal')}")
page.wait("[...document.querySelectorAll('.term-btns button')].some(b => b.textContent === 'Take control')", "the take-control button")
# The click asks confirm(), which blocks the page: click from a timer and
# accept the dialog.
page.js("setTimeout(() => [...document.querySelectorAll('.term-btns button')].find(b => b.textContent === 'Take control').click(), 0)")
page.wait_event("Page.javascriptDialogOpening")
page.send("Page.handleJavaScriptDialog", {"accept": True})
page.wait("document.querySelector('.keybar') && document.querySelector('.term-state.control') && document.querySelector('.xterm-helper-textarea:not([readonly])') && !document.querySelector('.term-msg')", "control mode")
time.sleep(0.5)
page.js("document.querySelector('.xterm-helper-textarea').focus()")
for ch in "hello from the check":
    page.send("Input.dispatchKeyEvent", {"type": "keyDown", "key": ch, "text": ch, "unmodifiedText": ch})
    page.send("Input.dispatchKeyEvent", {"type": "keyUp", "key": ch})
try:
    page.wait("(document.querySelector('.xterm-rows') || {}).textContent?.includes('hello from the check')", "typed text in the terminal")
except SystemExit:
    print("active element:", page.js("document.activeElement && document.activeElement.className"))
    print("rows:", page.js("(document.querySelector('.xterm-rows') || {}).textContent"))
    raise
open(os.path.join(out, "action-terminal.png"), "wb").write(base64.b64decode(page.send("Page.captureScreenshot", {"format": "png"})["data"]))
collect("actions: terminal")

print(f"{len(screens) * 4} screens, 4 action runs and the logic checks done; screenshots and DOM in {out}")
if problems:
    print("problems:")
    for p in problems:
        print("  " + p)
    sys.exit(1)
print("no errors, exceptions or policy violations")
PY

python3 -I "$WORK/cdp.py" "$URL" "$CDP_PORT" "$OUT" "$SESSION"
