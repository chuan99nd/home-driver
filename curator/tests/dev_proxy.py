"""Stand-in for Drive's admin proxy, for checking the UI in a real browser.

Serves the curator under /curator/ exactly like Drive does: strips the prefix and adds
``X-Home-Driver-Email``. Anything else answers with a tiny placeholder "Drive" page.

    uv run python -m tests.dev_proxy --port 8190 --curator http://127.0.0.1:8191
    open http://127.0.0.1:8190/curator/
"""

from __future__ import annotations

import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import httpx

PREFIX = "/curator"
HOP = {
    "connection",
    "keep-alive",
    "transfer-encoding",
    "content-encoding",
    "content-length",
    "host",
}


def make_handler(upstream: str, email: str) -> type[BaseHTTPRequestHandler]:
    client = httpx.Client(base_url=upstream, follow_redirects=False, timeout=60)

    class Handler(BaseHTTPRequestHandler):
        def _send(self, status: int, headers: list[tuple[str, str]], body: bytes) -> None:
            self.send_response(status)
            for name, value in headers:
                self.send_header(name, value)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _proxy(self) -> None:
            path = self.path
            if path == PREFIX:
                self._send(308, [("Location", PREFIX + "/")], b"")
                return
            if not path.startswith(PREFIX + "/"):
                page = (
                    b"<!doctype html><title>Drive (placeholder)</title>"
                    b'<a href="curator/">Curator</a>'
                )
                self._send(200, [("Content-Type", "text/html; charset=utf-8")], page)
                return
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else b""
            headers = {k: v for k, v in self.headers.items() if k.lower() not in HOP}
            headers["X-Home-Driver-Email"] = email
            headers["X-Forwarded-Host"] = self.headers.get("Host", "")
            resp = client.request(self.command, path[len(PREFIX) :], headers=headers, content=body)
            out = [(k, v) for k, v in resp.headers.multi_items() if k.lower() not in HOP]
            self._send(resp.status_code, out, resp.content)

        do_GET = do_POST = _proxy

        def log_message(self, format: str, *args: object) -> None:
            pass

    return Handler


def main() -> None:
    parser = argparse.ArgumentParser(description="Dev stand-in for Drive's /curator/ proxy")
    parser.add_argument("--port", type=int, default=8190)
    parser.add_argument("--curator", default="http://127.0.0.1:8191")
    parser.add_argument("--email", default="owner@example.com")
    args = parser.parse_args()
    server = ThreadingHTTPServer(("127.0.0.1", args.port), make_handler(args.curator, args.email))
    print(f"open http://127.0.0.1:{args.port}{PREFIX}/  (acting as {args.email})")
    server.serve_forever()


if __name__ == "__main__":
    main()
