import http.server
import os
import socketserver

PORT = os.environ.get('PORT', 3000)

class RequestHandler(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-type', 'application/json')
        self.end_headers()
        self.wfile.write(json.dumps({'healthy': True, 'load': 0}))

    def do_HEAD(self):
        self.send_response(200)
        self.end_headers()


print("listening on {}...".format(PORT))
socketserver.TCPServer(("", int(PORT)), RequestHandler).serve_forever()
