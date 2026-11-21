#!/usr/bin/env python3
"""LFENV_POST_SCRIPT that seeds a new LoopForge environment.

On the first successful start of an environment (LFENV_FIRST_RUN=1) it creates
the administrator, saves the administrator's model keys and GitHub token, sets
the shared CodeBuddy key, and creates projects bound to repositories. Later
rebuilds skip it. Every step tolerates existing data, so a run that failed
half-way is completed by the next build.

Secrets are read from LFENV_POST_CONFIG (default /etc/lfenv/post-init.json, see
post-init.example.json) and never printed.
"""
import json
import os
import sys
import urllib.error
import urllib.request

BASE = os.environ.get("LFENV_BASE_URL", "").rstrip("/")
CONFIG = os.environ.get("LFENV_POST_CONFIG", "/etc/lfenv/post-init.json")


class APIError(Exception):
    def __init__(self, status, message):
        super().__init__(f"HTTP {status} {message}".strip())
        self.status = status


class Client:
    def __init__(self, base):
        self.base = base
        self.cookie = ""
        self.opener = urllib.request.build_opener(NoRedirect)

    def call(self, method, path, body=None):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.base + path, data=data, method=method)
        # LoopForge accepts password and settings writes only from its own origin.
        req.add_header("Origin", self.base)
        req.add_header("Content-Type", "application/json")
        if self.cookie:
            req.add_header("Cookie", self.cookie)
        try:
            with self.opener.open(req, timeout=60) as res:
                for header in res.headers.get_all("Set-Cookie") or []:
                    pair = header.split(";", 1)[0]
                    if pair.startswith("agentflow_user=") and pair != "agentflow_user=":
                        self.cookie = pair
                raw = res.read()
        except urllib.error.HTTPError as e:
            try:
                message = json.loads(e.read() or b"{}").get("error", "")
            except ValueError:
                message = ""
            raise APIError(e.code, message) from None
        return json.loads(raw) if raw.strip() else None


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def log(message):
    print(f"post-init: {message}", flush=True)


def ensure_admin(c, admin):
    creds = {"login": admin["login"], "password": admin["password"]}
    try:
        user = c.call("POST", "/api/auth/register", creds)["user"]
        log(f"created administrator {admin['login']}")
    except APIError as e:
        if e.status != 409:
            raise
        user = c.call("POST", "/api/auth/password", creds)["user"]
        log(f"administrator {admin['login']} already exists, signed in")
    if not c.cookie:
        raise RuntimeError("sign-in returned no session cookie")
    return user["id"]


def save_model_keys(c, uid, cfg):
    update, names = {}, []
    codebuddy = cfg.get("codebuddy") or {}
    if codebuddy.get("apiKey"):
        update["codeBuddyApiKey"] = codebuddy["apiKey"]
        update["codeBuddyInternetEnvironment"] = codebuddy.get("environment", "")
        names.append(f"codebuddy({codebuddy.get('environment') or 'international'})")
    clis = {cli: {"apiKey": key} for cli, key in (cfg.get("agentCli") or {}).items() if key}
    if clis:
        update["agentCliCredentials"] = clis
        names += sorted(clis)
    for field, name in (("codexApiKey", "codex"), ("claudeCodeApiKey", "claude-code")):
        if cfg.get(field):
            update[field] = cfg[field]
            names.append(name)
    if update:
        c.call("PUT", f"/api/users/{uid}/credentials", update)
        log("saved model keys: " + ", ".join(names))


def save_github_token(c, uid, token):
    path = f"/api/users/{uid}/git-http-credentials"
    update = {"name": "GitHub", "platform": "github", "host": "github.com", "token": token}
    try:
        existing = c.call("GET", path) or []
    except APIError as e:
        if e.status not in (404, 405):
            raise
        # Versions before per-host credentials (v0.4.0 and older) keep one Git token per user.
        c.call("PUT", f"/api/users/{uid}/credentials", {"gitToken": token})
        log("saved GitHub token (single per-user Git token)")
        return
    for cred in existing:
        # Updating by ID keeps one credential for the host.
        if cred.get("host") == "github.com" and not cred.get("managed"):
            update.update(id=cred["id"], name=cred["name"], platform=cred["platform"], username=cred.get("username", ""))
            break
    c.call("PUT", path, update)
    log("saved GitHub token for github.com")


def save_shared_codebuddy(c, shared):
    env = shared.get("environment", "")
    catalog = c.call("GET", f"/api/platform/codebuddy-key?environment={env}").get("catalog") or []
    models = shared.get("models") or [m["id"] for m in catalog if m.get("id")][:32]
    if not models:
        raise RuntimeError(f"no CodeBuddy models are listed for environment {env!r}")
    c.call("PUT", "/api/platform/codebuddy-key", {"apiKey": shared["apiKey"], "internetEnvironment": env, "models": models})
    log(f"saved shared CodeBuddy key ({env or 'international'}, {len(models)} models)")


def repo_key(url):
    url = url.lower().strip().removesuffix("/").removesuffix(".git")
    return url.split("://", 1)[-1]


def ensure_project(c, uid, spec):
    pid, url = spec["id"], spec["repository"]
    projects = {p["id"]: p for p in c.call("GET", "/api/projects") or []}
    if pid not in projects:
        c.call("POST", "/api/projects", {"id": pid, "name": spec.get("name") or pid, "ownerId": uid})
        log(f"created project {pid}")
    repo = next((r for r in c.call("GET", "/api/repositories") or []
                 if r.get("projectId") == pid and repo_key(r.get("webUrl") or r.get("cloneUrl") or "") == repo_key(url)), None)
    if repo is None:
        repo = c.call("POST", "/api/repositories", {"projectId": pid, "repoUrl": url})
        log(f"added repository {repo.get('webUrl') or url} to {pid}")
    project = c.call("GET", f"/api/projects/{pid}")
    if project.get("defaultRepositoryId") != repo["id"]:
        project["defaultRepositoryId"] = repo["id"]
        c.call("PUT", "/api/projects", project)
        log(f"set default repository of {pid}")


def main():
    if os.environ.get("LFENV_FIRST_RUN") != "1":
        log("environment already initialized, skipped")
        return
    if not BASE:
        raise RuntimeError("LFENV_BASE_URL is not set")
    with open(CONFIG, encoding="utf-8") as f:
        cfg = json.load(f)
    c = Client(BASE)
    uid = ensure_admin(c, cfg["admin"])
    steps = [("model keys", lambda: save_model_keys(c, uid, cfg))]
    if cfg.get("githubToken"):
        steps.append(("GitHub token", lambda: save_github_token(c, uid, cfg["githubToken"])))
    if (cfg.get("sharedCodeBuddy") or {}).get("apiKey"):
        steps.append(("shared CodeBuddy key", lambda: save_shared_codebuddy(c, cfg["sharedCodeBuddy"])))
    for spec in cfg.get("projects") or []:
        steps.append((f"project {spec['id']}", lambda spec=spec: ensure_project(c, uid, spec)))
    skipped = []
    for name, step in steps:
        try:
            step()
        except APIError as e:
            # Tags and old commits predate some endpoints; seed what this version has.
            if e.status not in (404, 405):
                raise
            log(f"skipped {name}: this version has no such API (HTTP {e.status})")
            skipped.append(name)
    log("done" + (f", skipped {len(skipped)} step(s)" if skipped else ""))


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # noqa: BLE001 - report one line, never a traceback with request data
        print(f"post-init: failed: {e}", file=sys.stderr)
        sys.exit(1)
