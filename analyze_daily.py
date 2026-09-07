# -*- coding: utf-8 -*-
"""Analyze fetched tender records for the daily report."""
import json, time
from pathlib import Path
from datetime import datetime, timezone, timedelta

MSK = timezone(timedelta(hours=3))
ROOT = Path(__file__).parent
NOW = datetime.now(MSK)

def txt(v):
    """Extract plain text from bitable text/url field."""
    if v is None:
        return ""
    if isinstance(v, list):
        return "".join(seg.get("text", "") if isinstance(seg, dict) else str(seg) for seg in v).strip()
    if isinstance(v, dict):
        return (v.get("text") or v.get("link") or "").strip()
    return str(v).strip()

def num(v):
    if isinstance(v, (int, float)):
        return v
    try:
        return float(str(v).replace(" ", "").replace(",", "."))
    except Exception:
        return None

def dt(ms):
    if not ms:
        return None
    return datetime.fromtimestamp(ms / 1000, MSK)

import sys
stamp = sys.argv[1] if len(sys.argv) > 1 else NOW.strftime("%Y-%m-%d")
items = json.loads((ROOT / "daily" / "raw" / f"records_{stamp}.json").read_text(encoding="utf-8"))
recs = []
for it in items:
    f = it.get("fields", {})
    r = {
        "record_id": it.get("record_id"),
        "created": dt(it.get("created_time")),
        "modified": dt(it.get("last_modified_time")),
        "number": txt(f.get("Номер")),
        "name": txt(f.get("Наименование")) or txt(f.get("Kimi резюме")),
        "customer": txt(f.get("Заказчик")),
        "region": txt(f.get("Регион")),
        "amount": num(f.get("Сумма")),
        "deadline": dt(f.get("Срок подачи")),
        "url": txt(f.get("Ссылка")),
        "status": txt(f.get("Статус")),
        "funnel": txt(f.get("Воронка")),
        "tp_id": txt(f.get("TenderplanID")),
        "summary": txt(f.get("Kimi резюме")),
    }
    recs.append(r)

day = timedelta(days=1)
new24 = [r for r in recs if r["created"] and NOW - r["created"] <= day]
dl3 = [r for r in recs if r["deadline"] and NOW <= r["deadline"] <= NOW + timedelta(days=3)]
dl7 = [r for r in recs if r["deadline"] and NOW + timedelta(days=3) < r["deadline"] <= NOW + timedelta(days=7)]
review = [r for r in recs if r["status"] == "На рассмотрении"]
expired_dl = [r for r in recs if r["deadline"] and r["deadline"] < NOW and r["status"] in ("Новый", "На рассмотрении", "В работе")]

def fmt_amount(a):
    if a is None:
        return "—"
    if a >= 1_000_000:
        return f"{a/1_000_000:,.2f} млн ₽".replace(",", " ").replace(".", ",")
    return f"{a:,.0f} ₽".replace(",", " ")

def show(r):
    return {
        "№": r["number"], "заказчик": r["customer"], "регион": r["region"],
        "сумма": fmt_amount(r["amount"]),
        "дедлайн": r["deadline"].strftime("%Y-%m-%d %H:%M") if r["deadline"] else "—",
        "статус": r["status"], "воронка": r["funnel"],
        "создан": r["created"].strftime("%Y-%m-%d %H:%M") if r["created"] else "—",
        "url": r["url"], "резюме": r["summary"][:220],
    }

print("=== NOW:", NOW.strftime("%Y-%m-%d %H:%M %Z"))
print("=== TOTAL:", len(recs))
from collections import Counter
print("=== STATUS COUNTS:", dict(Counter(r["status"] or "—" for r in recs)))
print("=== FUNNEL COUNTS:", dict(Counter(r["funnel"] or "—" for r in recs)))
print("\n=== NEW LAST 24H:", len(new24))
for r in sorted(new24, key=lambda x: x["created"], reverse=True):
    print(json.dumps(show(r), ensure_ascii=False))
print("\n=== DEADLINE <= 3 DAYS:", len(dl3))
for r in sorted(dl3, key=lambda x: x["deadline"]):
    print(json.dumps(show(r), ensure_ascii=False))
print("\n=== DEADLINE 3-7 DAYS:", len(dl7))
for r in sorted(dl7, key=lambda x: x["deadline"]):
    print(json.dumps(show(r), ensure_ascii=False))
print("\n=== STATUS 'На рассмотрении':", len(review))
for r in sorted(review, key=lambda x: (x["deadline"] is None, x["deadline"])):
    print(json.dumps(show(r), ensure_ascii=False))
print("\n=== ACTIVE BUT DEADLINE PASSED:", len(expired_dl))
for r in sorted(expired_dl, key=lambda x: x["deadline"]):
    print(json.dumps(show(r), ensure_ascii=False))
print("\n=== ALL RECORDS (compact, sorted by deadline) ===")
def keyf(r):
    return (r["deadline"] is None, r["deadline"] or datetime.max.replace(tzinfo=MSK))
for r in sorted(recs, key=keyf):
    print(json.dumps({k: v for k, v in show(r).items() if k != "резюме"}, ensure_ascii=False))
