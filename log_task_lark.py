# -*- coding: utf-8 -*-
"""Log a task record into Lark Base 'проекты маркетинга' via tenant token.
Usage: python log_task_lark.py --task "..." --result "..." [--status "Выполнено"]
Reuses the already created base; creates it if missing. Never prints secrets."""
import argparse, json, time
from pathlib import Path
import requests

ROOT = Path(__file__).parent
BASE = "https://open.feishu.cn/open-apis"
KNOWN_APP = "UFZxb6PAja67uusOILqu2GR1tpf"    # base «проекты маркетинга» (created 2026-09-03)
KNOWN_TABLE = "tbl9UOQAtqEDvVMa"

def load_env(path):
    cfg = {}
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        cfg[k.strip().lower()] = v.strip().strip('"').strip("'")
    return cfg

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--task", required=True)
    ap.add_argument("--result", default="")
    ap.add_argument("--status", default="Выполнено")
    a = ap.parse_args()

    cfg = load_env(ROOT / ".env")
    tok = requests.post(f"{BASE}/auth/v3/tenant_access_token/internal",
                        json={"app_id": cfg["lark_app_id"], "app_secret": cfg["lark_app_secret"]},
                        timeout=30).json()
    if tok.get("code") != 0:
        print(json.dumps({"ok": False, "step": "token", "msg": tok.get("msg")}, ensure_ascii=False))
        return
    H = {"Authorization": f"Bearer {tok['tenant_access_token']}", "Content-Type": "application/json"}

    app_token, table_id = KNOWN_APP, KNOWN_TABLE
    chk = requests.get(f"{BASE}/bitable/v1/apps/{app_token}/tables/{table_id}/fields?page_size=20",
                       headers=H, timeout=30).json()
    if chk.get("code") != 0:
        cr = requests.post(f"{BASE}/bitable/v1/apps", headers=H,
                           json={"name": "проекты маркетинга"}, timeout=30).json()
        if cr.get("code") != 0:
            print(json.dumps({"ok": False, "step": "create_base", "msg": cr.get("msg")}, ensure_ascii=False))
            return
        app_token = cr["data"]["app"]["app_token"]
        tr = requests.get(f"{BASE}/bitable/v1/apps/{app_token}/tables?page_size=20", headers=H, timeout=30).json()
        table_id = tr["data"]["items"][0]["table_id"]

    rec = {"Задача": a.task, "Дата": int(time.time() * 1000),
           "Статус": a.status, "Результат": a.result}
    rr = requests.post(f"{BASE}/bitable/v1/apps/{app_token}/tables/{table_id}/records",
                       headers=H, json={"fields": rec}, timeout=30).json()
    print(json.dumps({"ok": rr.get("code") == 0, "msg": rr.get("msg"),
                      "app_token": app_token, "table_id": table_id}, ensure_ascii=False))

if __name__ == "__main__":
    main()
