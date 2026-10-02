#!/usr/bin/env python3
"""Vendor the subset of GitHub's official OpenAPI description the fake serves.

Source: github/rest-api-description, tag v2.1.0,
descriptions-next/api.github.com/api.github.com.json (OpenAPI 3.1 — the
variant that carries the webhook payload schemas as well as the REST ones).

    curl -sSfL -o /tmp/api.31.json \
      https://raw.githubusercontent.com/github/rest-api-description/v2.1.0/descriptions-next/api.github.com/api.github.com.json
    python3 extract_schemas.py /tmp/api.31.json | python3 -c "import json,sys;json.dump(json.load(sys.stdin),sys.stdout,separators=(',',':'),sort_keys=True)" > openapi-subset.json

The output keeps, for each REST operation the fake implements, its 2xx
response schema(s), and for each webhook it emits, its payload schema — plus
the transitive closure of every #/components/schemas entry they reference.
Nothing is edited: schemas are copied verbatim.
"""
import hashlib, json, sys

REST = [
    ("post", "/app/installations/{installation_id}/access_tokens"),
    ("get", "/app/installations"),
    ("get", "/repos/{owner}/{repo}/installation"),
    ("get", "/orgs/{org}/installation"),
    ("get", "/users/{username}/installation"),
    ("get", "/installation/repositories"),
    ("get", "/user"),
    ("get", "/repos/{owner}/{repo}/pulls"),
    ("post", "/repos/{owner}/{repo}/pulls"),
    ("get", "/repos/{owner}/{repo}/pulls/{pull_number}"),
    ("patch", "/repos/{owner}/{repo}/pulls/{pull_number}"),
    ("get", "/repos/{owner}/{repo}/pulls/{pull_number}/files"),
    ("get", "/repos/{owner}/{repo}/pulls/{pull_number}/commits"),
    ("put", "/repos/{owner}/{repo}/pulls/{pull_number}/merge"),
    ("get", "/repos/{owner}/{repo}/pulls/{pull_number}/requested_reviewers"),
    ("post", "/repos/{owner}/{repo}/pulls/{pull_number}/requested_reviewers"),
    ("delete", "/repos/{owner}/{repo}/pulls/{pull_number}/requested_reviewers"),
    ("get", "/repos/{owner}/{repo}/pulls/{pull_number}/reviews"),
    ("post", "/repos/{owner}/{repo}/pulls/{pull_number}/reviews"),
    ("get", "/repos/{owner}/{repo}/pulls/{pull_number}/reviews/{review_id}"),
    ("get", "/repos/{owner}/{repo}/pulls/{pull_number}/reviews/{review_id}/comments"),
    ("get", "/repos/{owner}/{repo}/pulls/{pull_number}/comments"),
    ("post", "/repos/{owner}/{repo}/pulls/{pull_number}/comments/{comment_id}/replies"),
    ("get", "/repos/{owner}/{repo}/pulls/comments/{comment_id}/reactions"),
    ("post", "/repos/{owner}/{repo}/pulls/comments/{comment_id}/reactions"),
    ("get", "/repos/{owner}/{repo}/issues"),
    ("post", "/repos/{owner}/{repo}/issues"),
    ("get", "/repos/{owner}/{repo}/issues/{issue_number}"),
    ("patch", "/repos/{owner}/{repo}/issues/{issue_number}"),
    ("get", "/repos/{owner}/{repo}/issues/{issue_number}/comments"),
    ("post", "/repos/{owner}/{repo}/issues/{issue_number}/comments"),
    ("post", "/repos/{owner}/{repo}/issues/{issue_number}/assignees"),
    ("delete", "/repos/{owner}/{repo}/issues/{issue_number}/assignees"),
    ("post", "/repos/{owner}/{repo}/issues/{issue_number}/labels"),
    ("delete", "/repos/{owner}/{repo}/issues/{issue_number}/labels/{name}"),
    ("get", "/repos/{owner}/{repo}/issues/comments/{comment_id}/reactions"),
    ("post", "/repos/{owner}/{repo}/issues/comments/{comment_id}/reactions"),
    ("get", "/search/issues"),
    ("post", "/repos/{owner}/{repo}/statuses/{sha}"),
    ("get", "/repos/{owner}/{repo}/commits/{ref}"),
    ("get", "/repos/{owner}/{repo}/commits/{ref}/status"),
    ("get", "/repos/{owner}/{repo}/commits/{ref}/statuses"),
    ("get", "/repos/{owner}/{repo}/commits/{ref}/check-runs"),
    ("get", "/repos/{owner}/{repo}/actions/runs"),
    ("get", "/repos/{owner}/{repo}/actions/runs/{run_id}"),
    ("get", "/repos/{owner}/{repo}/actions/jobs/{job_id}"),
    ("get", "/repos/{owner}/{repo}/contents/{path}"),
    ("put", "/repos/{owner}/{repo}/contents/{path}"),
    ("delete", "/repos/{owner}/{repo}/contents/{path}"),
    ("post", "/repos/{owner}/{repo}/git/refs"),
    ("post", "/repos/{owner}/{repo}/releases"),
    ("get", "/repos/{owner}/{repo}/releases/{release_id}"),
    ("post", "/repos/{owner}/{repo}/actions/runs/{run_id}/rerun"),
    ("post", "/repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs"),
    ("post", "/repos/{owner}/{repo}/actions/runs/{run_id}/cancel"),
    ("post", "/repos/{owner}/{repo}/actions/workflows/{workflow_id}/dispatches"),
    ("delete", "/repos/{owner}/{repo}/issues/comments/{comment_id}/reactions/{reaction_id}"),
    ("delete", "/repos/{owner}/{repo}/pulls/comments/{comment_id}/reactions/{reaction_id}"),
    ("post", "/repos/{owner}/{repo}/releases/{release_id}/assets"),
    ("get", "/gists"),
    ("post", "/gists"),
    ("get", "/gists/{gist_id}"),
    ("patch", "/gists/{gist_id}"),
    ("delete", "/gists/{gist_id}"),
    ("get", "/users/{username}/gists"),
]

WEBHOOKS = [
    "pull-request-opened", "pull-request-synchronize", "pull-request-closed",
    "pull-request-reopened", "pull-request-review-requested",
    "pull-request-ready-for-review", "pull-request-converted-to-draft",
    "pull-request-labeled", "pull-request-edited",
    "pull-request-review-submitted", "pull-request-review-edited",
    "pull-request-review-dismissed",
    "pull-request-review-comment-created", "pull-request-review-comment-edited",
    "pull-request-review-thread-resolved", "pull-request-review-thread-unresolved",
    "issue-comment-created", "issues-opened", "issues-labeled", "issues-assigned",
    "issues-closed", "issues-reopened", "pull-request-assigned", "projects-v2-item-edited",
    "check-run-completed", "check-suite-completed", "workflow-run-completed",
    "status", "push", "release-published", "deployment-status-created",
    "dependabot-alert-created", "secret-scanning-alert-created",
]

def main(path):
    raw = open(path, "rb").read()
    d = json.loads(raw)
    comps = d["components"]["schemas"]
    out = {
        "source": "github/rest-api-description@v2.1.0 descriptions-next/api.github.com/api.github.com.json",
        "openapi": d["openapi"], "info_version": d["info"]["version"],
        "sha256": hashlib.sha256(raw).hexdigest(),
        "operations": {}, "webhooks": {}, "schemas": {},
    }
    need = set()

    def refs(node):
        if isinstance(node, dict):
            r = node.get("$ref")
            if isinstance(r, str) and r.startswith("#/components/schemas/"):
                name = r.rsplit("/", 1)[1]
                if name not in need:
                    need.add(name)
                    refs(comps[name])
            for v in node.values():
                refs(v)
        elif isinstance(node, list):
            for v in node:
                refs(v)

    def resolve_resp(r):
        if "$ref" in r:
            return d["components"]["responses"][r["$ref"].rsplit("/", 1)[1]]
        return r

    for method, p in REST:
        op = d["paths"][p][method]
        got = {}
        for code, r in op["responses"].items():
            if not code.startswith("2"):
                continue
            r = resolve_resp(r)
            c = r.get("content") or {}
            sch = (c.get("application/json") or {}).get("schema")
            got[code] = sch  # None for an empty-body 2xx (204)
            if sch:
                refs(sch)
        out["operations"][method.upper() + " " + p] = got
    for name in WEBHOOKS:
        sch = d["webhooks"][name]["post"]["requestBody"]["content"]["application/json"]["schema"]
        out["webhooks"][name] = sch
        refs(sch)
    for n in sorted(need):
        out["schemas"][n] = comps[n]
    json.dump(out, sys.stdout, indent=1, sort_keys=True)

main(sys.argv[1])
