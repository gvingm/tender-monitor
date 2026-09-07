# -*- coding: utf-8 -*-
"""Fix up the freshly created 'проекты маркетинга' base: Russian field names,
proper field types, and fill the first log record completely."""
import json, time
from pathlib import Path
import requests

ROOT = Path(__file__).parent
APP = "UFZxb6PAja67uusOILqu2GR1tpf"
TABLE = "tbl9UOQAtqEDvVMa"
BASE = "https://open.feishu.cn/open-apis"

def load_env(path):
    cfg = {}
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        cfg[k.strip()] = v.strip().strip('"').strip("'")
    return cfg

cfg = load_env(ROOT / ".env")
app_id = next(v for k, v in cfg.items() if k.lower() == "lark_app_id")
app_secret = next(v for k, v in cfg.items() if k.lower() == "lark_app_secret")
tok = requests.post(f"{BASE}/auth/v3/tenant_access_token/internal",
                    json={"app_id": app_id, "app_secret": app_secret}, timeout=30).json()
H = {"Authorization": f"Bearer {tok['tenant_access_token']}", "Content-Type": "application/json"}

log = []

# current fields
fr = requests.get(f"{BASE}/bitable/v1/apps/{APP}/tables/{TABLE}/fields?page_size=100", headers=H, timeout=30).json()
items = fr["data"]["items"]
by_type = {f["field_name"]: f for f in items}

def rename_field(fid, name, ftype, prop=None):
    body = {"field_name": name, "type": ftype}
    if prop:
        body["property"] = prop
    r = requests.put(f"{BASE}/bitable/v1/apps/{APP}/tables/{TABLE}/fields/{fid}",
                     headers=H, json=body, timeout=30).json()
    log.append({"op": f"rename->{name}", "code": r.get("code"), "msg": r.get("msg")})
    return r.get("code") == 0

for f in items:
    if f["type"] == 1:  # text -> Задача
        rename_field(f["field_id"], "Задача", 1)
    elif f["type"] == 3:  # single select -> Статус
        rename_field(f["field_id"], "Статус", 3,
                     {"options": [{"name": "Выполнено"}, {"name": "В работе"}, {"name": "Ошибка"}]})
    elif f["type"] == 5:  # date -> Дата
        rename_field(f["field_id"], "Дата", 5, {"date_formatter": "yyyy-MM-dd"})
    elif f["type"] == 17:  # attachment -> delete
        r = requests.delete(f"{BASE}/bitable/v1/apps/{APP}/tables/{TABLE}/fields/{f['field_id']}",
                            headers=H, timeout=30).json()
        log.append({"op": "delete attachment field", "code": r.get("code"), "msg": r.get("msg")})

# add Результат text field
r = requests.post(f"{BASE}/bitable/v1/apps/{APP}/tables/{TABLE}/fields",
                  headers=H, json={"field_name": "Результат", "type": 1}, timeout=30).json()
log.append({"op": "add Результат", "code": r.get("code"), "msg": r.get("msg")})

# update the single existing record
lr = requests.get(f"{BASE}/bitable/v1/apps/{APP}/tables/{TABLE}/records?page_size=10",
                  headers=H, timeout=30).json()
recs = lr["data"]["items"]
now_ms = int(time.time() * 1000)
result_txt = ("Отчёт daily/2026-09-03.md: 75 записей, новых 16, дедлайн<3дн: 12, «На рассмотрении»: 0. "
              "Топ: АП123121 (220,05 млн ₽, 04.09), АП123151 (39,76 млн ₽, 03.09), АП123100 (31,38 млн ₽, 18.09). "
              "Kimi-резюме сломано (ошибка API).")
for rec in recs:
    ur = requests.put(f"{BASE}/bitable/v1/apps/{APP}/tables/{TABLE}/records/{rec['record_id']}",
                      headers=H, json={"fields": {
                          "Задача": "Ежедневный анализ тендеров Дидал СК — 2026-09-03",
                          "Дата": now_ms,
                          "Статус": "Выполнено",
                          "Результат": result_txt}}, timeout=30).json()
    log.append({"op": f"update record {rec['record_id']}", "code": ur.get("code"), "msg": ur.get("msg")})

print(json.dumps({"log": log}, ensure_ascii=False))
