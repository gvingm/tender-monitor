# -*- coding: utf-8 -*-
"""Publish daily/YYYY-MM-DD.md as a Lark Docx via Drive import, share link,
optionally post the link to the Lark group chat. Never prints secrets."""
import json, sys, time
from pathlib import Path
import requests

ROOT = Path(__file__).parent
BASE = "https://open.feishu.cn/open-apis"
FOLDER_NAME = "Дидал СК — тендерные сводки"
LARK_CHAT_ID = "oc_6cc3a4c2e69b74e6a7d240c1e95db951"  # default from main.go

def load_env(path):
    cfg = {}
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        cfg[k.strip().lower()] = v.strip().strip('"').strip("'")
    return cfg

def main(md_path, post_to_chat=False):
    cfg = load_env(ROOT / ".env")
    tok = requests.post(f"{BASE}/auth/v3/tenant_access_token/internal",
                        json={"app_id": cfg["lark_app_id"], "app_secret": cfg["lark_app_secret"]},
                        timeout=30).json()
    if tok.get("code") != 0:
        return {"ok": False, "step": "token", "code": tok.get("code"), "msg": tok.get("msg")}
    H = {"Authorization": f"Bearer {tok['tenant_access_token']}"}

    md = Path(md_path)
    stamp = md.stem  # YYYY-MM-DD
    title = f"Тендерная сводка Дидал СК — {stamp}"

    # root folder
    r = requests.get(f"{BASE}/drive/explorer/v2/root_folder/meta", headers=H, timeout=30).json()
    if r.get("code") != 0:
        return {"ok": False, "step": "root_folder", "code": r.get("code"), "msg": r.get("msg")}
    root_token = r["data"]["token"]

    # find or create folder
    folder_token = None
    r = requests.get(f"{BASE}/drive/v1/files", headers=H,
                     params={"folder_token": root_token, "page_size": 200}, timeout=30).json()
    if r.get("code") == 0:
        for f in r["data"].get("files", []):
            if f.get("type") == "folder" and f.get("name") == FOLDER_NAME:
                folder_token = f["token"]
                break
    if not folder_token:
        r = requests.post(f"{BASE}/drive/v1/files/create_folder", headers=H,
                          json={"name": FOLDER_NAME, "folder_token": root_token}, timeout=30).json()
        if r.get("code") != 0:
            return {"ok": False, "step": "create_folder", "code": r.get("code"), "msg": r.get("msg")}
        folder_token = r["data"]["token"]

    # upload md file
    data_bytes = md.read_bytes()
    r = requests.post(f"{BASE}/drive/v1/medias/upload_all", headers=H,
                      data={"file_name": md.name, "parent_type": "explorer",
                            "parent_node": folder_token, "size": str(len(data_bytes))},
                      files={"file": (md.name, data_bytes, "text/markdown")}, timeout=60).json()
    if r.get("code") != 0:
        return {"ok": False, "step": "upload", "code": r.get("code"), "msg": r.get("msg")}
    file_token = r["data"]["file_token"]

    # import md -> docx
    r = requests.post(f"{BASE}/drive/v1/import_tasks", headers=H,
                      json={"file_extension": "md", "file_token": file_token, "type": "docx",
                            "file_name": title,
                            "point": {"mount_type": 1, "mount_key": folder_token}}, timeout=30).json()
    if r.get("code") != 0:
        return {"ok": False, "step": "import", "code": r.get("code"), "msg": r.get("msg")}
    ticket = r["data"]["ticket"]

    doc_token = None
    for _ in range(30):
        time.sleep(2)
        r = requests.get(f"{BASE}/drive/v1/import_tasks/{ticket}", headers=H, timeout=30).json()
        if r.get("code") != 0:
            return {"ok": False, "step": "poll", "code": r.get("code"), "msg": r.get("msg")}
        res = r["data"]["result"]
        if res.get("job_status") == 0:
            doc_token = res["token"]
            break
        if res.get("job_status") not in (1, 2):  # 1/2 = in progress
            return {"ok": False, "step": "import_job", "job_status": res.get("job_status"),
                    "job_error": res.get("job_error_msg")}
    if not doc_token:
        return {"ok": False, "step": "poll_timeout"}

    url = f"https://feishu.cn/docx/{doc_token}"

    # public link share: anyone with the link can read (no Lark login)
    share = requests.patch(
        f"{BASE}/drive/v1/permissions/{doc_token}/public?type=docx", headers=H,
        json={"external_access_entity": "open", "link_share_entity": "anyone_readable",
              "share_entity": "anyone", "invite_external": True}, timeout=30).json()

    chat_posted = None
    if post_to_chat:
        m = requests.post(f"{BASE}/im/v1/messages?receive_id_type=chat_id", headers=H,
                          json={"receive_id": LARK_CHAT_ID, "msg_type": "text",
                                "content": json.dumps({"text": f"📊 {title}\n{url}"}, ensure_ascii=False)},
                          timeout=30).json()
        chat_posted = {"code": m.get("code"), "msg": m.get("msg")}

    return {"ok": True, "url": url, "doc_token": doc_token, "folder_token": folder_token,
            "share": {"code": share.get("code"), "msg": share.get("msg")},
            "chat": chat_posted}

if __name__ == "__main__":
    md = sys.argv[1] if len(sys.argv) > 1 else str(ROOT / "daily" / (time.strftime("%Y-%m-%d") + ".md"))
    post = "--post-chat" in sys.argv
    print(json.dumps(main(md, post), ensure_ascii=False))
