from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
from tempfile import TemporaryDirectory
from threading import Thread
from unittest import TestCase
from unittest.mock import patch

from chains_automation.services.ntfy import Ntfy


class NtfyTest(TestCase):
    def setUp(self):
        self.requests = []
        self.status = 200
        self.response = {"id": "message1", "topic": "house", "time": 1770000000, "message": "private"}
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                data = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                fixture.requests.append((self.path, self.headers.get("Authorization"), data))
                self.send_response(fixture.status)
                if fixture.status == 302:
                    self.send_header("Location", "/another-origin")
                self.end_headers()
                self.wfile.write(json.dumps(fixture.response).encode())

            def log_message(self, *args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        worker = Thread(target=server.serve_forever, daemon=True)
        worker.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.url = f"http://127.0.0.1:{server.server_port}"

    def service(self, **settings):
        service = Ntfy(settings={"server_url": self.url, "topics": {"alerts": "house"}, "token": "private", **settings})
        service.onInit()
        return service

    def test_registered_schema_and_publish(self):
        service = self.service()
        info = service.describe()
        self.assertEqual(info["devices"][0]["id"], "notifications")
        schema = info["actions"]["notify"]["1"]
        self.assertEqual(schema["properties"]["topic"]["enum"], ["alerts"])
        self.assertEqual(schema["required"], ["message"])
        receipt = service.action_notify("Door open", priority=4, tags=["warning"], click="https://example.test/view")
        self.assertEqual(receipt, {"id": "message1", "topic": "alerts", "time": 1770000000})
        self.assertEqual(self.requests, [("/", "Bearer private", {
            "topic": "house", "message": "Door open", "priority": 4,
            "tags": ["warning"], "click": "https://example.test/view",
        })])

    def test_rejects_unknown_topics_and_redacts_http_failures(self):
        service = self.service()
        with self.assertRaisesRegex(ValueError, "configured ntfy topic"):
            service.action_notify("Door open", topic="unlisted")
        with self.assertRaisesRegex(ValueError, "4096 UTF-8 bytes"):
            service.action_notify("æ" * 4096)
        self.assertEqual(self.requests, [])
        for status in (302, 401, 429, 503):
            self.status = status
            with self.assertRaisesRegex(ValueError, fr"^ntfy rejected the publish \(HTTP {status}\)$"):
                service.action_notify("Door open")
        self.assertEqual(len(self.requests), 4)
        self.status = 200
        self.response = {"topic": "house", "message": "private"}
        with self.assertRaisesRegex(ValueError, "^ntfy returned an invalid publish receipt$"):
            service.action_notify("Door open")

    def test_toml_and_environment(self):
        with TemporaryDirectory() as directory, patch.dict(os.environ, {"CHAINS_SETTINGS__TOKEN": "env-token"}):
            path = Path(directory) / "ntfy.toml"
            path.write_text(f'[settings]\nserver_url = "{self.url}"\n'
                            'token = "file-token"\ndefault_topic = "home"\n'
                            '[settings.topics]\nhome = "house"\n')
            service = Ntfy(config_path=path)
            service.onInit()
            self.assertEqual(service.action_notify("Door open")["topic"], "home")
            self.assertEqual(self.requests[0][1], "Bearer env-token")
