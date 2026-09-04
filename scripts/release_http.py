#!/usr/bin/env python3
"""Synchronous HTTP contract-2 client. Durable per-node requests survive CI disconnects."""
import argparse
import contextlib
import datetime
import fcntl
import json
import os
from pathlib import Path
import re
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

TERMINAL = {"succeeded", "skipped", "failed"}
MAX_RESPONSE = 4 * 1024 * 1024


class ReleaseError(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class HTTP:
    def __init__(self, token, timeout=420):
        if not token:
            raise ReleaseError("RELEASE_TOKEN is required")
        self.token, self.timeout = token, timeout
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def call(self, base, path, body=None):
        url = base.rstrip("/") + path
        data = json.dumps(body, separators=(",", ":")).encode() if body is not None else None
        request = urllib.request.Request(url, data=data, headers={"X-Release-Token": self.token, "Content-Type": "application/json"})
        try:
            response = self.opener.open(request, timeout=self.timeout)
        except urllib.error.HTTPError as exc:
            response = exc
        except (OSError, urllib.error.URLError, TimeoutError) as exc:
            raise ReleaseError("HTTP outcome unknown for " + base + ": " + str(exc)) from exc
        with response:
            raw = response.read(MAX_RESPONSE + 1)
            if len(raw) > MAX_RESPONSE:
                raise ReleaseError("HTTP response exceeds limit")
            try:
                result = json.loads(raw)
            except (ValueError, UnicodeError) as exc:
                raise ReleaseError("HTTP response is not JSON") from exc
            if not isinstance(result, dict):
                raise ReleaseError("HTTP response is not an object")
            return response.code, result


def atomic_save(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix=".batch-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(data, stream, ensure_ascii=False, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        parent = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(parent)
        finally:
            os.close(parent)
    finally:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(name)


def check_url(url):
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ReleaseError("invalid service URL (credentials, query and fragment prohibited)")
    return url.rstrip("/")


def state_path(env, target_id=None, release_id=None, request=None):
    query = {"env": env}
    if target_id:
        query["target_id"] = target_id
    else:
        query.update(type=request["type"], project=request.get("project", ""), **request["params"])
    if release_id:
        query["release_id"] = release_id
    return "/api/v1/releases/state?" + urllib.parse.urlencode(query)


def require(code, result, expected=200):
    if code != expected:
        raise ReleaseError("HTTP %s: %s" % (code, result.get("error_code", result.get("error", "request failed"))))
    return result


def require_frontend_capability(health, request):
    if request.get("type") == "frontend_static" and "frontend_oras_v1" not in health.get("capabilities", []):
        raise ReleaseError("node does not support pinned ORAS frontend artifacts")


def preflight(http, urls, request):
    nodes, seen = [], set()
    for url in urls:
        url = check_url(url)
        health = require(*http.call(url, "/healthz"))
        if health.get("release_contract") != 2 or health.get("publish_ready") is not True:
            raise ReleaseError(url + " is not ready for HTTP release contract 2")
        require_frontend_capability(health, request)
        node_id = health.get("node_id")
        if not node_id or node_id in seen or request["env"] not in health.get("enabled_envs", [health.get("env")]):
            raise ReleaseError("duplicate/missing node_id or environment mismatch: " + url)
        seen.add(node_id)
        baseline = require(*http.call(url, state_path(request["env"], request=request)))
        if baseline.get("recovery_required") or baseline.get("active_release_id") or baseline.get("node_id") != node_id:
            raise ReleaseError("node has unresolved state: " + url)
        if not baseline.get("state_revision") or not baseline.get("target_id"):
            raise ReleaseError("missing contract-2 state fields: " + url)
        req = dict(request, release_id=str(uuid.uuid4()), expected_state_revision=baseline["state_revision"])
        nodes.append({"url": url, "node_id": node_id, "target_id": baseline["target_id"], "before": baseline, "request": req, "phase": "prepared"})
    return nodes


def identity(http, node, env):
    health = require(*http.call(node["url"], "/healthz"))
    require_frontend_capability(health, node["request"])
    if health.get("release_contract") != 2 or health.get("node_id") != node["node_id"] or env not in health.get("enabled_envs", [health.get("env")]):
        raise ReleaseError("endpoint identity or contract changed: " + node["url"])


def execute_node(http, batch, node, path, key="request", result_key="result", resolve_timeout=120):
    request = node[key]
    result = node.get(result_key)
    if result and result.get("status") in TERMINAL:
        return result
    identity(http, node, batch["env"])
    deadline = time.monotonic() + resolve_timeout
    sent = False
    while True:
        # Query first: after a crash an earlier POST may have completed successfully.
        code, live = http.call(node["url"], state_path(batch["env"], node["target_id"], request["release_id"]))
        if code == 200:
            result = live.get("release", {})
            if result.get("release_id") != request["release_id"]:
                raise ReleaseError("state response release identity mismatch")
            if result.get("status") in TERMINAL:
                node[result_key] = result
                node["phase"] = result_key + "_complete"
                atomic_save(path, batch)
                return result
            if result.get("status") == "recovery_required":
                node[result_key] = result
                node["phase"] = "recovery_required"
                atomic_save(path, batch)
                raise ReleaseError("node requires recovery: " + node["url"])
        elif code == 404 and not sent:
            # Write the exact ID/parameters before issuing the only side-effecting operation.
            node["phase"] = key + "_sending"
            atomic_save(path, batch)
            sent = True
            try:
                status, response = http.call(node["url"], "/api/v1/releases/apply", request)
            except ReleaseError:
                node["phase"] = "unknown"
                atomic_save(path, batch)
            else:
                if response.get("release_id") not in {None, request["release_id"]}:
                    raise ReleaseError("POST response release identity mismatch")
                # A terminal response is trusted only when tied to an accepted record.
                if response.get("status") in TERMINAL and response.get("state_revision_before"):
                    node[result_key] = response
                    node["phase"] = result_key + "_complete"
                    atomic_save(path, batch)
                    return response
                if status in {400, 401, 403, 409, 413} and response.get("error_code") != "RELEASE_RUNNING":
                    node["phase"] = "rejected"
                    node["rejection"] = response
                    atomic_save(path, batch)
                    raise ReleaseError("release rejected: " + response.get("error_code", str(status)))
                if response.get("status") == "recovery_required":
                    node[result_key] = response
                    atomic_save(path, batch)
                    raise ReleaseError("node requires recovery: " + node["url"])
            continue
        elif code != 404:
            require(code, live)
        if time.monotonic() >= deadline:
            node["phase"] = "unknown"
            atomic_save(path, batch)
            raise ReleaseError("node outcome unresolved; resume the same batch: " + node["url"])
        time.sleep(1)


def restore_batch(http, batch, path, resolve_timeout=120):
    batch["phase"] = "restoring"
    atomic_save(path, batch)
    # Resolve unknown writes before starting ANY restoration.
    for node in batch["nodes"]:
        if node.get("phase") in {"prepared", "rejected"}:
            continue
        if node.get("result", {}).get("status") not in TERMINAL:
            execute_node(http, batch, node, path, resolve_timeout=resolve_timeout)
    for node in reversed(batch["nodes"]):
        result = node.get("result", {})
        if result.get("status") != "succeeded":
            continue
        if "restore_request" not in node:
            identity(http, node, batch["env"])
            live = require(*http.call(node["url"], state_path(batch["env"], node["target_id"], node["request"]["release_id"])))
            baseline = live.get("baseline")
            if not baseline:
                raise ReleaseError("no restorable pre-release snapshot on " + node["url"])
            if live["state_revision"] != result["state_revision_after"] or live.get("current_commit_id") != result["commit_id"]:
                raise ReleaseError("baseline changed; later deployment must not be overwritten: " + node["url"])
            node["restore_request"] = dict(node["request"], release_id=str(uuid.uuid4()), expected_state_revision=result["state_revision_after"], restore_of=node["request"]["release_id"], commit_id=baseline["commit_id"], version=baseline["version"])
            if node["request"]["type"] == "frontend_static":
                if not re.fullmatch(r"sha256:[0-9a-f]{64}", baseline.get("artifact_digest", "")):
                    raise ReleaseError("baseline is missing the original artifact digest")
                node["restore_request"]["artifact_digest"] = baseline["artifact_digest"]
            atomic_save(path, batch)
        restored = execute_node(http, batch, node, path, "restore_request", "restore_result", resolve_timeout)
        if restored.get("status") != "succeeded":
            raise ReleaseError("restoration failed on " + node["url"])
    batch["phase"] = "restored"
    atomic_save(path, batch)


def update_batch(http, batch, path, resolve_timeout=120):
    if batch.get("phase") in {"restoring", "restored"}:
        raise ReleaseError("batch is being restored; use rollback to continue")
    batch["phase"] = "applying"
    atomic_save(path, batch)
    try:
        for node in batch["nodes"]:
            result = execute_node(http, batch, node, path, resolve_timeout=resolve_timeout)
            if result.get("status") not in {"succeeded", "skipped"}:
                raise ReleaseError("publication failed on " + node["url"])
    except ReleaseError as exc:
        batch["phase"], batch["error"] = "incomplete", str(exc)
        atomic_save(path, batch)
        if batch["failure_policy"] == "restore":
            restore_batch(http, batch, path, resolve_timeout)
        raise
    batch["phase"] = "succeeded"
    batch.pop("error", None)
    atomic_save(path, batch)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("update", "resume", "rollback"), nargs="?", default="update")
    parser.add_argument("--batch-file", default=os.getenv("RELEASE_BATCH_FILE", "release-batch.json"))
    parser.add_argument("--timeout", type=float, default=420)
    parser.add_argument("--resolve-timeout", type=float, default=120)
    parser.add_argument("--failure-policy", choices=("stop", "restore"), default=os.getenv("RELEASE_FAILURE_POLICY", "stop"))
    args = parser.parse_args()
    if args.timeout <= 0 or args.resolve_timeout <= 0:
        raise ReleaseError("timeouts must be positive")
    http = HTTP(os.getenv("RELEASE_TOKEN"), args.timeout)
    path = Path(args.batch_file)
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(str(path) + ".lock", "a") as guard:
        try:
            fcntl.flock(guard, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise ReleaseError("another client owns this batch file") from exc
        if path.exists():
            if args.action == "update":
                raise ReleaseError("batch file exists; use resume or a new --batch-file")
            batch = json.loads(path.read_text())
            if batch.get("schema") != 2 or not batch.get("nodes"):
                raise ReleaseError("not a contract-2 batch record")
            for node in batch["nodes"]:
                check_url(node["url"])
        else:
            if args.action != "update":
                raise ReleaseError("resume/rollback requires the original batch file")
            values = {key: os.getenv(key, "").strip() for key in ("RELEASE_URLS", "RELEASE_ENV", "RELEASE_TYPE", "RELEASE_BRANCH", "RELEASE_COMMIT", "RELEASE_PATH_DEST", "RELEASE_SERVER_NAME")}
            if values["RELEASE_TYPE"] == "frontend_static" and not values["RELEASE_BRANCH"]:
                values["RELEASE_BRANCH"] = "artifact"
            if not all(values.values()):
                raise ReleaseError("required environment: " + ", ".join(key for key, value in values.items() if not value))
            if not re.fullmatch(r"(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})", values["RELEASE_COMMIT"]):
                raise ReleaseError("RELEASE_COMMIT must be a full commit ID")
            request = {"env": values["RELEASE_ENV"], "type": values["RELEASE_TYPE"], "branch": values["RELEASE_BRANCH"], "commit_id": values["RELEASE_COMMIT"].lower(), "project": os.getenv("RELEASE_PROJECT", ""), "version": os.getenv("RELEASE_VERSION", ""), "operator": os.getenv("BUILD_USER_ID", ""), "build_url": os.getenv("BUILD_URL", ""), "params": {"path_dest": values["RELEASE_PATH_DEST"], "server_name": values["RELEASE_SERVER_NAME"]}}
            if request["type"] == "frontend_static":
                digest = os.getenv("RELEASE_ARTIFACT_DIGEST", "").strip()
                if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                    raise ReleaseError("RELEASE_ARTIFACT_DIGEST must be a pinned sha256 OCI digest")
                request["artifact_digest"] = digest
            urls = [url for url in re.split(r"[\s,]+", values["RELEASE_URLS"]) if url]
            nodes = preflight(http, urls, request)
            if args.failure_policy == "restore" and any(not node["before"].get("current") for node in nodes):
                raise ReleaseError("restore policy requires an existing baseline on every node; first deployment must use stop policy")
            batch = {"schema": 2, "batch_id": str(uuid.uuid4()), "env": request["env"], "failure_policy": args.failure_policy, "phase": "prepared", "started_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "nodes": nodes}
            atomic_save(path, batch)
        if args.action == "rollback":
            restore_batch(http, batch, path, args.resolve_timeout)
        else:
            update_batch(http, batch, path, args.resolve_timeout)
        print("%s: %s" % (path, batch["phase"]))


if __name__ == "__main__":
    try:
        main()
    except (ReleaseError, OSError, ValueError, KeyError) as error:
        raise SystemExit("release failed: " + str(error))
