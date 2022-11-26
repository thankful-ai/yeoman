import http.server
import os
import socketserver


PORT = os.environ.get('PORT', 3000)


class RequestHandler(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()

    def do_HEAD(self):
        self.send_response(200)
        self.end_headers()


print("listening on {}...".format(PORT))
socketserver.TCPServer(("", int(PORT)), RequestHandler).serve_forever()
