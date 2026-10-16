"""Serves the desktop app's frontend for review in an ordinary browser.

Opened without the app around it, the page runs against its built-in mock
backend, so every screen can be looked at without building anything.
Responses say "don't cache", so a reload always shows the files as they are
on disk.

    python scripts/serve-ui.py [port]
"""
import http.server
import os
import sys

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'app', 'desktop', 'frontend')


class NoCache(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=ROOT, **kwargs)

    def end_headers(self):
        self.send_header('Cache-Control', 'no-store')
        super().end_headers()


if __name__ == '__main__':
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 5173
    http.server.ThreadingHTTPServer(('127.0.0.1', port), NoCache).serve_forever()
