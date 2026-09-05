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
        self.capabilities = []
        self.digest_by_node = {"http://a": "sha256:" + "d" * 64, "http://b": "sha256:" + "e" * 64}
        self.recovery_required = False
        for name, old in (("http://a", "a" * 40), ("http://b", "b" * 40)):
            self.nodes[name] = {"revision": str(uuid.uuid4()), "commit": old, "records": {}, "target": name[-1] * 64}

    def call(self, base, path, body=None):
        node = self.nodes[base]
        if path == "/healthz":
            return 200, {"release_contract": 1 if base == self.old_contract else 2, "status": "ok", "node_id": base[-1], "env": "test", "publish_ready": True, "capabilities": self.capabilities}
        if body is None:
            query = parse_qs(urlsplit(path).query)
            release_id = query.get("release_id", [None])[0]
            if release_id and self.drop_query:
                self.drop_query = False
                raise client.ReleaseError("offline")
            state = {"node_id": base[-1], "state_revision": node["revision"], "target_id": node["target"], "current_commit_id": node["commit"], "current": {"commit_id": node["commit"]}, "recovery_required": self.recovery_required}
            if release_id:
                if release_id not in node["records"]:
                    return 404, {}
                state.update(node["records"][release_id])
            return 200, state
        self.calls.append((base, copy.deepcopy(body)))
        if path in {"/api/v1/releases/nginx/test", "/api/v1/releases/nginx/reload"}:
            release_id = body["release_id"]
            record = node["records"].get(release_id)
            if not record:
                return 404, {"error_code": "RELEASE_NOT_FOUND"}
            result = record["release"]
            if path.endswith("/test"):
                if base == self.fail_node and not record["restore"]:
                    result.update(self.failure, status="failed", rollback_status="succeeded")
                    node["commit"] = record["baseline"]["commit_id"]
                    node["revision"] = str(uuid.uuid4())
                    result["state_revision_after"] = node["revision"]
                    return 500, result
                result.update(status="succeeded", phase="nginx_test_succeeded", activation_status="nginx_test_passed")
                return 200, result
            if result.get("phase") != "nginx_test_succeeded":
                return 409, {"error_code": "NGINX_RELOAD_NOT_AVAILABLE"}
            node["commit"] = record["candidate"]
            node["revision"] = str(uuid.uuid4())
            result.update(status="succeeded", phase="complete", activation_status="reload_requested", state_revision_after=node["revision"])
            return 200, result
        if body["expected_state_revision"] != node["revision"]:
            return 409, {"error_code": "STATE_REVISION_CONFLICT"}
        before = node["commit"]
        result = {"release_id": body["release_id"], "state_revision_before": node["revision"], "commit_id": body["commit_id"], "status": "succeeded", "phase": "latest_switched", "activation_status": "latest_switched"}
        if body["type"] == "frontend_static":
            result["artifact_digest"] = body.get("artifact_digest", self.digest_by_node[base])
        result["state_revision_after"] = node["revision"]
        node["records"][body["release_id"]] = {"release": result, "candidate": body["commit_id"], "restore": "restore_of" in body, "baseline": {"commit_id": before, "version": before, "artifact_digest": "sha256:" + before[0] * 64}}
        node["commit"] = body["commit_id"]
        if self.drop_reply:
            self.drop_reply = False
            raise client.ReleaseError("response lost")
        return 200, result


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

    def publish_calls(self):
        return [(url, request) for url, request in self.http.calls if "expected_state_revision" in request]

    def test_old_server_preflight_never_posts(self):
        self.http.old_contract = "http://b"
        with self.assertRaises(client.ReleaseError):
            batch(self.http)
        self.assertEqual(self.http.calls, [])

    def test_stale_recovery_flag_does_not_block_preflight(self):
        self.http.recovery_required = True
        request = {"env": "test", "type": "config", "branch": "main", "commit_id": "c" * 40, "params": {"server_name": "site", "path_dest": "/publish"}}
        nodes = client.preflight(self.http, ["http://a", "http://b"], request)
        self.assertEqual(len(nodes), 2)
        self.assertEqual(self.http.calls, [])

    def test_response_lost_resolves_same_id_without_second_publish(self):
        self.http.drop_reply = True
        client.update_batch(self.http, self.batch, self.path)
        self.assertEqual(len(self.http.calls), 6)
        self.assertEqual(len(self.publish_calls()), 2)
        self.assertEqual(self.batch["phase"], "succeeded")
        recovered = json.loads(self.path.read_text())
        client.update_batch(self.http, recovered, self.path)
        self.assertEqual(len(self.http.calls), 6)

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
        self.assertEqual(len(self.http.calls), 6)

    def test_partial_failure_stops_and_restores_only_successful_nodes(self):
        self.http.fail_node = "http://b"
        self.batch["failure_policy"] = "restore"
        with self.assertRaises(client.ReleaseError):
            client.update_batch(self.http, self.batch, self.path)
        self.assertEqual(self.batch["phase"], "restored")
        self.assertEqual(self.http.nodes["http://a"]["commit"], "a" * 40)
        self.assertEqual(len(self.http.calls), 8)
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
        self.assertEqual([url for url, _ in self.http.calls], ["http://a", "http://a"])

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
        self.assertEqual([url for url, _ in http.calls], ["http://a", "http://a"])

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
        self.assertEqual(len(self.http.calls), 6)

    def test_frontend_requires_oras_capability_before_any_post(self):
        request = dict(self.batch["nodes"][0]["request"], type="frontend_static", artifact_digest="sha256:" + "a" * 64)
        with self.assertRaisesRegex(client.ReleaseError, "ORAS"):
            client.preflight(self.http, ["http://a"], request)
        self.assertEqual(self.http.calls, [])

    def frontend_batch(self):
        self.http.capabilities = ["frontend_oras_v1", "request_targets_v1"]
        request = dict(self.batch["nodes"][0]["request"], type="frontend_static")
        request.pop("branch", None)
        return dict(self.batch, nodes=client.preflight(self.http, ["http://a", "http://b"], request))

    def test_frontend_sha_only_pins_remaining_nodes_and_replays_original_request(self):
        current = self.frontend_batch()
        self.http.drop_reply = True
        client.update_batch(self.http, current, self.path)
        first, second = self.publish_calls()
        self.assertNotIn("artifact_digest", first[1])
        self.assertEqual(second[1]["artifact_digest"], self.http.digest_by_node["http://a"])
        saved = json.loads(self.path.read_text())
        self.assertEqual(saved["artifact_digest"], second[1]["artifact_digest"])
        self.assertNotIn("artifact_digest", saved["nodes"][0]["request"])
        client.update_batch(self.http, saved, self.path)
        self.assertEqual(len(self.http.calls), 6)
        client.restore_batch(self.http, saved, self.path)
        restores = {url: req["artifact_digest"] for url, req in self.http.calls if "restore_of" in req}
        self.assertEqual(restores, {"http://a": "sha256:" + "a" * 64, "http://b": "sha256:" + "b" * 64})

    def test_frontend_resume_after_crash_before_batch_digest_was_recorded(self):
        current = self.frontend_batch()
        client.execute_node(self.http, current, current["nodes"][0], self.path)
        saved = json.loads(self.path.read_text())
        self.assertNotIn("artifact_digest", saved)
        client.update_batch(self.http, saved, self.path)
        self.assertEqual(len(self.http.calls), 6)
        self.assertEqual(self.publish_calls()[1][1]["artifact_digest"], self.http.digest_by_node["http://a"])

    def test_frontend_never_rewrites_an_unresolved_request_to_add_digest(self):
        current = self.frontend_batch()
        current["artifact_digest"] = self.http.digest_by_node["http://a"]
        current["nodes"][0]["phase"] = "unknown"
        with self.assertRaisesRegex(client.ReleaseError, "cannot change an unresolved"):
            client.update_batch(self.http, current, self.path)
        self.assertEqual(self.http.calls, [])

    def test_frontend_digest_mismatch_stops_before_next_node(self):
        current = self.frontend_batch()
        expected = "sha256:" + "f" * 64
        current["nodes"][0]["request"]["artifact_digest"] = expected
        original = self.http.call
        def wrong_digest(base, path, body=None):
            code, result = original(base, path, body)
            if body is not None:
                result["artifact_digest"] = self.http.digest_by_node[base]
            return code, result
        with patch.object(self.http, "call", side_effect=wrong_digest):
            with self.assertRaisesRegex(client.ReleaseError, "digest differs"):
                client.update_batch(self.http, current, self.path)
        self.assertEqual([url for url, _ in self.http.calls], ["http://a", "http://a", "http://a"])

    def test_frontend_sha_only_requires_resolution_capability_before_any_post(self):
        request = dict(self.batch["nodes"][0]["request"], type="frontend_static")
        self.http.capabilities = ["frontend_oras_v1"]
        with self.assertRaisesRegex(client.ReleaseError, "SHA tag resolution"):
            client.preflight(self.http, ["http://a"], request)
        self.assertEqual(self.http.calls, [])

    def run_main(self, action, environment, path):
        with patch.dict(os.environ, environment, clear=True), patch.object(sys, "argv", ["release_http.py", action, "--batch-file", str(path)]), patch.object(client, "HTTP", return_value=self.http), patch("builtins.print"):
            client.main()

    def test_jenkins_environment_reaches_http_for_all_release_types(self):
        for typ, branch in (("config", "feature/new-config"), ("whitelist", "prod"), ("config", ""), ("frontend_static", "")):
            with self.subTest(typ=typ, branch=branch):
                self.http = FakeHTTP()
                self.http.capabilities = ["frontend_oras_v1", "request_targets_v1"]
                env = {"RELEASE_ENV": "test", "RELEASE_TYPE": typ, "RELEASE_BRANCH": branch, "RELEASE_COMMIT": "C" * 40, "RELEASE_SERVER_NAME": "another-site", "RELEASE_PATH_DEST": "/data/custom-root", "RELEASE_URLS": "http://a,\nhttp://b", "RELEASE_PROJECT": "custom-project", "RELEASE_TOKEN": "test-token"}
                path = Path(self.temp.name) / (str(uuid.uuid4()) + ".json")
                self.run_main("update", env, path)
                request = self.http.calls[0][1]
                self.assertEqual(request["type"], typ)
                self.assertEqual(request["env"], "test")
                self.assertEqual(request["commit_id"], "c" * 40)
                self.assertEqual(request["project"], "custom-project")
                self.assertEqual(request["params"], {"server_name": "another-site", "path_dest": "/data/custom-root"})
                if branch:
                    self.assertEqual(request["branch"], branch)
                else:
                    self.assertNotIn("branch", request)
                self.assertNotIn("artifact_digest", request)
                self.assertNotIn("test-token", path.read_text())

    def test_resume_rejects_environment_mismatch_before_any_post(self):
        with self.assertRaisesRegex(client.ReleaseError, "original batch environment"):
            self.run_main("resume", {"RELEASE_ENV": "other", "RELEASE_TOKEN": "test-token"}, self.path)
        self.assertEqual(self.http.calls, [])

    def test_resume_preserves_original_parameters_despite_new_jenkins_inputs(self):
        original = copy.deepcopy(self.batch["nodes"][0]["request"])
        env = {"RELEASE_ENV": "test", "RELEASE_TYPE": "frontend_static", "RELEASE_BRANCH": "wrong-branch", "RELEASE_COMMIT": "d" * 40, "RELEASE_SERVER_NAME": "wrong-site", "RELEASE_PATH_DEST": "/wrong", "RELEASE_URLS": "http://wrong", "RELEASE_TOKEN": "test-token"}
        self.run_main("resume", env, self.path)
        self.assertEqual(self.http.calls[0][1], original)
        self.assertEqual([url for url, _ in self.publish_calls()], ["http://a", "http://b"])

    def test_empty_service_list_is_rejected_before_preflight(self):
        env = {"RELEASE_ENV": "test", "RELEASE_TYPE": "config", "RELEASE_COMMIT": "c" * 40, "RELEASE_SERVER_NAME": "site", "RELEASE_PATH_DEST": "/data/root", "RELEASE_URLS": ", ,", "RELEASE_TOKEN": "test-token"}
        with self.assertRaisesRegex(client.ReleaseError, "at least one"):
            self.run_main("update", env, Path(self.temp.name) / "empty.json")
        self.assertEqual(self.http.calls, [])

    def test_tokens_not_written_to_batch(self):
        client.update_batch(self.http, self.batch, self.path)
        self.assertNotIn("release_token", self.path.read_text().lower())
        self.assertNotIn("x-release-token", self.path.read_text().lower())


if __name__ == "__main__":
    unittest.main()
