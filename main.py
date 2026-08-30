# FastAPI Tender Monitor with Lark Bitable + Kimi

"""
Tender Monitor — ежедневный мониторинг тендеров Tenderplan + Lark Bitable + Kimi-резюме
"""

from fastapi import FastAPI, HTTPException, Request
from apscheduler.schedulers.asyncio import AsyncIOScheduler
from apscheduler.triggers.cron import CronTrigger
from datetime import datetime
from dotenv import load_dotenv
import httpx
import json
import os

# Load .env
load_dotenv(dotenv_path=os.path.join(os.path.dirname(__file__), ".env"))

app = FastAPI(title="Tender Monitor")
scheduler = AsyncIOScheduler()

# === CONFIG ===
TENDERPLAN_API = "https://tenderplan.ru/api"
TENDERPLAN_KEY = os.getenv("TENDERPLAN_KEY")  # 128 chars
KIMI_API = "https://api.moonshot.ai/v1/chat/completions"
KIMI_MODEL = "kimi-k2.6"  # или kimi-k2.7-code-highspeed, kimi-k3
KIMI_KEY = os.getenv("KIMI_KEY")  # берётся из .env

LARK_APP_ID = os.getenv("LARK_APP_ID", "cli_aa1810fe6f78c079")
LARK_APP_SECRET = os.getenv("LARK_APP_SECRET", "")
LARK_BITABLE_APP = "X37KbBltZaqSSdsGyJdumNLFtmh"
LARK_BITABLE_TABLE = "tblalXw0gAIi3pVc"
LARK_CHAT_ID = "oc_6cc3a4c2e69b74e6a7d240c1e95db951"
LARK_TENANT_TOKEN = None  # cache

# Mail Assistant
MAIL_SECRET = os.getenv("MAIL_SECRET", "")

# Кластеры ключевых слов
CLUSTERS = {
    "Росморпорт": ["Росморпорт", "морской порт", "акватория", "дноуглубление", "берегоукрепление", "гидротехника", "причал"],
    "Малый техфлот": ["земснаряд", "буксир", "шаланда", "понтон", "катер", "моторная яхта", "маломерное судно"],
    "Иное": ["дноуглубление", "дноуглубительные работы", "судостроение", "гидротехника", "аренда флота"],
}


# === LARK AUTH ===
async def get_tenant_token() -> str:
    """Получить tenant_access_token (cache на 2 часа)"""
    global LARK_TENANT_TOKEN
    if LARK_TENANT_TOKEN:
        return LARK_TENANT_TOKEN

    async with httpx.AsyncClient() as client:
        r = await client.post(
            "https://open.larksuite.com/open-apis/auth/v3/tenant_access_token/internal",
            json={"app_id": LARK_APP_ID, "app_secret": LARK_APP_SECRET}
        )
        data = r.json()
        LARK_TENANT_TOKEN = data["tenant_access_token"]
        return LARK_TENANT_TOKEN


# === TENDERPLAN ===
async def fetch_tenders(cluster_name: str, keywords: list, since: str = None) -> list:
    """Получить тендеры по кластеру"""
    async with httpx.AsyncClient() as client:
        r = await client.get(
            f"{TENDERPLAN_API}/tenders/getlist",
            params={"key": TENDERPLAN_KEY, "q": " OR ".join(keywords), "since": since},
            timeout=30,
        )
        return r.json().get("data", [])


# === KIMI ===
async def kimi_summarize(tender: dict) -> str:
    """Генерация резюме через Kimi K2.6"""
    prompt = f"""Сделай краткое резюме (5-7 строк) этого тендера:

Название: {tender.get('name', '')}
Заказчик: {tender.get('customer', '')}
Описание: {tender.get('description', '')}
Сумма: {tender.get('amount', '')} ₽
Регион: {tender.get('region', '')}
Срок подачи: {tender.get('deadline', '')}

Формат:
[Суть закупки одним предложением]
Заказчик: [тип и регион]
Сумма: [₽]
Срок: [дата]
Ключевые требования: [3-4 пункта]
Наша оценка шансов: [высокие/средние/низкие]
Рекомендация: [подавать/не подавать]"""

    async with httpx.AsyncClient() as client:
        r = await client.post(
            KIMI_API,
            headers={"Authorization": f"Bearer {KIMI_KEY}"},
            json={
                "model": KIMI_MODEL,
                "messages": [{"role": "user", "content": prompt}],
                "temperature": 0.3,
            },
            timeout=60,
        )
        return r.json()["choices"][0]["message"]["content"]


# === LARK BITABLE ===
async def add_to_bitable(tender: dict, cluster: str, summary: str) -> dict:
    """Добавить запись в Bitable"""
    token = await get_tenant_token()
    fields = {
        "Номер": tender.get("number", ""),
        "Заказчик": cluster,
        "Регион": tender.get("region", ""),
        "Сумма": tender.get("amount", 0),
        "Срок подачи": int(datetime.fromisoformat(tender["deadline"]).timestamp() * 1000) if tender.get("deadline") else None,
        "Статус": "Новый",
        "Ссылка": {"text": tender.get("url", ""), "link": tender.get("url", "")},
        "Kimi резюме": summary,
    }

    async with httpx.AsyncClient() as client:
        # Сначала проверим, нет ли уже такого
        search_r = await client.post(
            f"https://open.larksuite.com/open-apis/bitable/v1/apps/{LARK_BITABLE_APP}/tables/{LARK_BITABLE_TABLE}/records/search",
            headers={"Authorization": f"Bearer {token}"},
            json={"filter": {"and": [{"field_name": "Номер", "operator": "is", "value": [fields["Номер"]]}]}},
            timeout=30,
        )
        if search_r.json().get("data", {}).get("items"):
            return {"status": "duplicate", "record": search_r.json()["data"]["items"][0]}

        # Создать запись
        r = await client.post(
            f"https://open.larksuite.com/open-apis/bitable/v1/apps/{LARK_BITABLE_APP}/tables/{LARK_BITABLE_TABLE}/records",
            headers={"Authorization": f"Bearer {token}"},
            json={"fields": fields},
            timeout=30,
        )
        return r.json()


# === LARK NOTIFICATION ===
async def notify_lark(tender: dict, cluster: str, summary: str) -> dict:
    """Отправить уведомление в группу"""
    token = await get_tenant_token()
    text = f"""🚢 Новый тендер «{cluster}»

💰 Сумма: {tender.get('amount', '?')} ₽
📍 Регион: {tender.get('region', '?')}
📅 Срок: {tender.get('deadline', '?')}
🔗 {tender.get('url', '')}

{summary}

#тендер #{cluster.lower().replace(' ', '_')}"""

    async with httpx.AsyncClient() as client:
        r = await client.post(
            f"https://open.larksuite.com/open-apis/im/v1/messages?receive_id_type=chat_id",
            headers={"Authorization": f"Bearer {token}"},
            json={
                "receive_id": LARK_CHAT_ID,
                "msg_type": "text",
                "content": json.dumps({"text": text}),
            },
            timeout=30,
        )
        return r.json()


# === MAIN WORKFLOW ===
async def process_tenders():
    """Главный workflow — запуск каждый день в 8:00"""
    results = {"processed": 0, "added": 0, "duplicates": 0, "errors": []}

    for cluster_name, keywords in CLUSTERS.items():
        tenders = await fetch_tenders(cluster_name, keywords)
        for tender in tenders:
            try:
                summary = await kimi_summarize(tender)
                result = await add_to_bitable(tender, cluster_name, summary)
                if result.get("status") == "duplicate":
                    results["duplicates"] += 1
                else:
                    results["added"] += 1
                    await notify_lark(tender, cluster_name, summary)
                results["processed"] += 1
            except Exception as e:
                results["errors"].append(str(e))

    return results


# === ENDPOINTS ===
@app.on_event("startup")
async def startup():
    scheduler.add_job(process_tenders, CronTrigger(hour=8, minute=0))
    scheduler.start()


@app.get("/")
async def root():
    return {"status": "ok", "service": "tender-monitor"}


@app.post("/run")
async def run_now():
    """Ручной запуск"""
    return await process_tenders()


@app.get("/tenders")
async def list_tenders(status: str = None):
    """Список тендеров"""
    token = await get_tenant_token()
    async with httpx.AsyncClient() as client:
        r = await client.post(
            f"https://open.larksuite.com/open-apis/bitable/v1/apps/{LARK_BITABLE_APP}/tables/{LARK_BITABLE_TABLE}/records/search",
            headers={"Authorization": f"Bearer {token}"},
            json={"filter": {"and": [{"field_name": "Статус", "operator": "is", "value": [status]}]}} if status else {},
            timeout=30,
        )
        return r.json()


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8787)
# Mail Assistant Webhook (added for tender integration)


@app.post("/webhook/mail-tenders")
async def mail_tenders_webhook(request: Request):
    """Получить тендеры от Mail Assistant → Bitable + Kimi + Lark"""
    body = await request.body()
    signature = request.headers.get("X-Mail-Signature", "")

    # 1. HMAC verify
    if MAIL_SECRET and not verify_hmac(body, signature, MAIL_SECRET):
        raise HTTPException(401, "Bad signature")

    # 2. Parse
    data = await request.json()
    tenders = data.get("tenders", [])

    results = {"added": 0, "duplicates": 0, "errors": []}
    for tender in tenders:
        try:
            cluster = classify_cluster(tender)
            summary = await kimi_summarize(tender)
            result = await add_to_bitable(tender, cluster, summary)
            if result.get("status") == "duplicate":
                results["duplicates"] += 1
            else:
                results["added"] += 1
                await notify_lark(tender, cluster, summary)
        except Exception as e:
            results["errors"].append(str(e))

    return results


def verify_hmac(body: bytes, signature: str, secret: str) -> bool:
    """HMAC SHA256 verify"""
    import hashlib
    import hmac as hmac_mod
    expected = hmac_mod.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac_mod.compare_digest(signature, expected)


def classify_cluster(tender: dict) -> str:
    """Определить кластер тендера по ключевым словам"""
    text = (tender.get("title", "") + " " + tender.get("description", "")).lower()
    for cluster, keywords in CLUSTERS.items():
        if any(kw.lower() in text for kw in keywords):
            return cluster
    return "Иное"
