# -*- coding: utf-8 -*-
"""Fetch all records from Lark Bitable using tenant_access_token from .env.
Secrets are never printed. Output: records JSON + meta info."""
import json, os, sys, time
from pathlib import Path

import requests

ROOT = Path(__file__).parent
ENV = ROOT / ".env"

def load_env(path):
    cfg = {}
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        k = k.strip()
        v = v.strip().strip('"').strip("'")
        cfg[k] = v
    return cfg

def pick(cfg, *names):
    for n in names:
        for k, v in cfg.items():
            if k.lower() == n.lower() and v:
                return v
    return None

def main():
    if not ENV.exists():
        print(json.dumps({"ok": False, "error": "env_missing"}, ensure_ascii=False))
        sys.exit(1)
    cfg = load_env(ENV)
    app_id = pick(cfg, "LARK_APP_ID")
    app_secret = pick(cfg, "LARK_APP_SECRET")
    # defaults from main.go (defaultBitableApp / defaultBitableTable)
    app_token = pick(cfg, "LARK_BITABLE_APP", "LARK_BITABLE_APP_TOKEN", "BITABLE_APP") or "X37KbBltZaqSSdsGyJdumNLFtmh"
    table_id = pick(cfg, "LARK_BITABLE_TABLE", "LARK_BITABLE_TABLE_ID", "BITABLE_TABLE") or "tblalXw0gAIi3pVc"
    missing = [n for n, v in [("app_id", app_id), ("app_secret", app_secret),
                              ("app_token", app_token), ("table_id", table_id)] if not v]
    if missing:
        print(json.dumps({"ok": False, "error": "env_keys_missing", "missing": missing}, ensure_ascii=False))
        sys.exit(1)

    # 1. tenant_access_token
    r = requests.post(
        "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal",
        json={"app_id": app_id, "app_secret": app_secret}, timeout=30)
    tok = r.json()
    if tok.get("code") != 0:
        print(json.dumps({"ok": False, "error": "token_failed", "code": tok.get("code"),
                          "msg": tok.get("msg")}, ensure_ascii=False))
        sys.exit(1)
    token = tok["tenant_access_token"]

    # 2. records with pagination
    url = f"https://open.feishu.cn/open-apis/bitable/v1/apps/{app_token}/tables/{table_id}/records"
    headers = {"Authorization": f"Bearer {token}"}
    items, page_token = [], None
    while True:
        params = {"page_size": 500, "automatic_fields": "true"}
        if page_token:
            params["page_token"] = page_token
        resp = requests.get(url, headers=headers, params=params, timeout=60)
        data = resp.json()
        if data.get("code") != 0:
            print(json.dumps({"ok": False, "error": "list_failed", "code": data.get("code"),
                              "msg": data.get("msg")}, ensure_ascii=False))
            sys.exit(1)
        d = data["data"]
        items.extend(d.get("items", []))
        if d.get("has_more"):
            page_token = d.get("page_token")
            time.sleep(0.2)
        else:
            break

    out = ROOT / "daily" / "raw"
    out.mkdir(parents=True, exist_ok=True)
    stamp = time.strftime("%Y-%m-%d")
    fpath = out / f"records_{stamp}.json"
    fpath.write_text(json.dumps(items, ensure_ascii=False, indent=1), encoding="utf-8")
    # field names overview
    fieldnames = sorted({fn for it in items for fn in it.get("fields", {}).keys()})
    print(json.dumps({"ok": True, "total": len(items), "file": str(fpath),
                      "fields": fieldnames}, ensure_ascii=False))

if __name__ == "__main__":
    main()
