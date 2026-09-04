import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

spec = importlib.util.spec_from_file_location("release_http", Path(__file__).with_name("release_http.py"))
client = importlib.util.module_from_spec(spec)
spec.loader.exec_module(client)


class FakeHTTP:
    def __init__(self):
        self.nodes = {}
        self.calls = []
        self.drop_reply = False
        self.drop_query = False
        self.fail_node = None
        self.failure = {"error_code": "NGINX_TEST_FAILED", "error": 'nginx -t: nginx: [emerg] unknown directive "bad_directive" in site.conf:7'}
        self.old_contract = None
        for name, old in (("http://a", "a" * 40), ("http://b", "b" * 40)):
            self.nodes[name] = {"revision": str(uuid.uuid4()), "commit": old, "records": {}, "target": name[-1] * 64}

    def call(self, base, path, body=None):
        node = self.nodes[base]
        if path == "/healthz":
            return 200, {"release_contract": 1 if base == self.old_contract else 2, "node_id": base[-1], "env": "test", "publish_ready": True}
        if body is None:
            query = parse_qs(urlsplit(path).query)
            release_id = query.get("release_id", [None])[0]
            if release_id and self.drop_query:
                self.drop_query = False
                raise client.ReleaseError("offline")
            state = {"node_id": base[-1], "state_revision": node["revision"], "target_id": node["target"], "current_commit_id": node["commit"], "current": {"commit_id": node["commit"]}, "recovery_required": False}
            if release_id:
                if release_id not in node["records"]:
                    return 404, {}
                state.update(node["records"][release_id])
            return 200, state
        self.calls.append((base, copy.deepcopy(body)))
        if body["expected_state_revision"] != node["revision"]:
            return 409, {"error_code": "STATE_REVISION_CONFLICT"}
        before = node["commit"]
        result = {"release_id": body["release_id"], "state_revision_before": node["revision"], "commit_id": body["commit_id"], "status": "succeeded"}
        if base == self.fail_node and "restore_of" not in body:
            result["status"] = "failed"
            result.update(self.failure)
        else:
            node["commit"] = body["commit_id"]
            node["revision"] = str(uuid.uuid4())
        result["state_revision_after"] = node["revision"]
        node["records"][body["release_id"]] = {"release": result, "baseline": {"commit_id": before, "version": before}}
        if self.drop_reply:
            self.drop_reply = False
            raise client.ReleaseError("response lost")
        return (500 if result["status"] == "failed" else 200), result


def batch(http):
    request = {"env": "test", "type": "config", "branch": "main", "commit_id": "c" * 40, "params": {"server_name": "site", "path_dest": "/publish"}}
    return {"schema": 2, "env": "test", "failure_policy": "stop", "phase": "prepared", "nodes": client.preflight(http, ["http://a", "http://b"], request)}


class BatchTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / "batch.json"
        self.http = FakeHTTP()
        self.batch = batch(self.http)
        client.atomic_save(self.path, self.batch)

    def test_old_server_preflight_never_posts(self):
        self.http.old_contract = "http://b"
        with self.assertRaises(client.ReleaseError):
            batch(self.http)
        self.assertEqual(self.http.calls, [])

    def test_response_lost_resolves_same_id_without_second_publish(self):
        self.http.drop_reply = True
        client.update_batch(self.http, self.batch, self.path)
        self.assertEqual(len(self.http.calls), 2)
        self.assertEqual(self.batch["phase"], "succeeded")
        recovered = json.loads(self.path.read_text())
        client.update_batch(self.http, recovered, self.path)
        self.assertEqual(len(self.http.calls), 2)

    def test_each_node_restores_its_own_baseline(self):
        client.update_batch(self.http, self.batch, self.path)
        client.restore_batch(self.http, self.batch, self.path)
        self.assertEqual(self.http.nodes["http://a"]["commit"], "a" * 40)
        self.assertEqual(self.http.nodes["http://b"]["commit"], "b" * 40)
        restores = [request for _, request in self.http.calls if "restore_of" in request]
        self.assertEqual({r["commit_id"] for r in restores}, {"a" * 40, "b" * 40})

    def test_later_deployment_stops_restore(self):
        client.update_batch(self.http, self.batch, self.path)
        self.http.nodes["http://b"]["revision"] = str(uuid.uuid4())
        with self.assertRaisesRegex(client.ReleaseError, "baseline changed"):
            client.restore_batch(self.http, self.batch, self.path)
        self.assertEqual(len(self.http.calls), 2)

    def test_partial_failure_stops_and_restores_only_successful_nodes(self):
        self.http.fail_node = "http://b"
        self.batch["failure_policy"] = "restore"
        with self.assertRaises(client.ReleaseError):
            client.update_batch(self.http, self.batch, self.path)
        self.assertEqual(self.batch["phase"], "restored")
        self.assertEqual(self.http.nodes["http://a"]["commit"], "a" * 40)
        self.assertEqual(len(self.http.calls), 3)
        self.assertEqual(self.http.calls[-1][0], "http://a")

    def test_nginx_error_stops_next_node_and_preserves_diagnostics_on_resume(self):
        self.http.fail_node = "http://a"
        with self.assertRaisesRegex(client.ReleaseError, "NGINX_TEST_FAILED.*bad_directive.*site.conf:7"):
            client.update_batch(self.http, self.batch, self.path)
        saved = json.loads(self.path.read_text())
        self.assertEqual(saved["phase"], "incomplete")
        self.assertIn(self.http.failure["error"], saved["error"])
        with self.assertRaisesRegex(client.ReleaseError, "bad_directive"):
            client.update_batch(self.http, saved, self.path)
        self.assertEqual([url for url, _ in self.http.calls], ["http://a"])

    def test_cli_exits_nonzero_and_prints_nginx_error(self):
        http = self.http
        http.fail_node = "http://a"

        class Handler(BaseHTTPRequestHandler):
            def reply(self, body=None):
                _, name, path = self.path.split("/", 2)
                code, result = http.call("http://" + name, "/" + path, body)
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(result).encode())

            def do_GET(self):
                self.reply()

            def do_POST(self):
                self.reply(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))

            def log_message(self, *args):
                pass

        with ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
            for node in self.batch["nodes"]:
                node["url"] = "http://127.0.0.1:%s/%s" % (server.server_port, node["node_id"])
            client.atomic_save(self.path, self.batch)
            thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True)
            thread.start()
            try:
                env = dict(os.environ, RELEASE_TOKEN="ci-test-token")
                run = subprocess.run([sys.executable, str(Path(__file__).with_name("release_http.py")), "resume", "--batch-file", str(self.path)], env=env, capture_output=True, text=True, timeout=10)
            finally:
                server.shutdown()
                thread.join(timeout=2)
        self.assertEqual(run.returncode, 1, run.stderr)
        self.assertIn("NGINX_TEST_FAILED", run.stderr)
        self.assertIn(http.failure["error"], run.stderr)
        self.assertEqual([url for url, _ in http.calls], ["http://a"])

    def test_batch_must_be_durable_before_post(self):
        with patch.object(client, "atomic_save", side_effect=OSError("disk full")):
            with self.assertRaises(OSError):
                client.execute_node(self.http, self.batch, self.batch["nodes"][0], self.path)
        self.assertEqual(self.http.calls, [])

    def test_unknown_query_does_not_start_any_rollback(self):
        client.update_batch(self.http, self.batch, self.path)
        self.batch["nodes"][1].pop("result")
        self.batch["nodes"][1]["phase"] = "unknown"
        self.http.drop_query = True
        with self.assertRaises(client.ReleaseError):
            client.restore_batch(self.http, self.batch, self.path)
        self.assertEqual(len(self.http.calls), 2)

    def test_frontend_requires_oras_capability_before_any_post(self):
        request = dict(self.batch["nodes"][0]["request"], type="frontend_static", artifact_digest="sha256:" + "a" * 64)
        with self.assertRaisesRegex(client.ReleaseError, "ORAS"):
            client.preflight(self.http, ["http://a"], request)
        self.assertEqual(self.http.calls, [])

    def test_tokens_not_written_to_batch(self):
        client.update_batch(self.http, self.batch, self.path)
        self.assertNotIn("release_token", self.path.read_text().lower())
        self.assertNotIn("x-release-token", self.path.read_text().lower())


if __name__ == "__main__":
    unittest.main()
