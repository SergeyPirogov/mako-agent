"""Serves Mako's board through 127.0.0.1:7790 with the sidebar prototype injected; everything else goes to :7777."""
import http.server, urllib.request, pathlib, urllib.parse
HERE = pathlib.Path(__file__).parent
UP = "http://127.0.0.1:7777"
import json
# Demo only: the live board has one project with tasks, so some move to two other real projects.
MOVE = {"SergeyPirogov/mako-agent#294": "market/tests/market-autotests", 
        "SergeyPirogov/mako-agent#152": "market/tests/market-autotests", "SergeyPirogov/mako-agent#42": "market/tests/market-autotests",
        "SergeyPirogov/mako-agent#296": "market/tests/market-autotests",
        "SergeyPirogov/mako-agent#292": "ecom/tests/moq", "SergeyPirogov/mako-agent#181": "ecom/tests/moq",
        "SergeyPirogov/mako-agent#150": "ecom/tests/moq", "SergeyPirogov/mako-agent#300": "ecom/tests/moq", "SergeyPirogov/mako-agent#306": "ecom/tests/moq"}
DEMO = [False]   # set by the last page load: /?demo=projects
def demo(body):
    d = json.loads(body)
    for r in d.get("records", []):
        if r["id"] in MOVE:
            r["task"]["project"] = MOVE[r["id"]]
    return json.dumps(d).encode()
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        if u.path in ("/proto.css", "/proto.js", "/select.js", "/scrolltest.js", "/foldshot.js"):
            body = (HERE / u.path[1:]).read_bytes()
            ct = "text/css" if u.path.endswith(".css") else "text/javascript"
        else:
            with urllib.request.urlopen(UP + self.path) as r:
                body, ct = r.read(), r.headers.get("Content-Type", "")
            if u.path == "/api/state" and DEMO[0]:
                body = demo(body)
            if u.path == "/" and "text/html" in ct:
                q = urllib.parse.parse_qs(u.query)
                DEMO[0] = q.get("demo") == ["projects"]
                inj = '<script src="/select.js"></script>'
                if q.get("mode") == ["after"]:
                    inj = '<link rel="stylesheet" href="/proto.css">' + inj + '<script src="/proto.js"></script><script src="/scrolltest.js"></script><script src="/foldshot.js"></script>'
                body = body.replace(b"</body>", inj.encode() + b"</body>")
        self.send_response(200); self.send_header("Content-Type", ct); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        self.send_response(204); self.end_headers()   # the prototype never changes Mako
http.server.ThreadingHTTPServer(("127.0.0.1", 7790), H).serve_forever()
