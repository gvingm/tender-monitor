# Bitable → Meegle Integration

## Концепция
При создании записи в Bitable "Тендеры" → автоматически создаётся задача в Meegle.

## Текущие ограничения
Lark MCP через `mcporter` экспортирует только Bitable/Docs/Sheets tools. Meegle API (через `meegle.com`) не экспортирован.

## Решение

### Вариант 1: Meegle Webhook (если есть)
Meegle поддерживает webhook на создание задач. Настроить:
- URL: `https://your-dokploy.com/webhook/meegle-new-task`
- Event: `task.created`

### Вариант 2: Прямой Meegle API через FastAPI
Реализовать в `main.py`:
```python
import httpx

MEEGLE_BASE = "https://api.meegle.com"
MEEGLE_TOKEN = os.getenv("MEEGLE_TOKEN")
MEEGLE_PROJECT_KEY = "didal-tenders"  # project в Meegle

async def create_meegle_task(tender: dict, cluster: str, summary: str) -> dict:
    async with httpx.AsyncClient() as client:
        r = await client.post(
            f"{MEEGLE_BASE}/open-api/{MEEGLE_PROJECT_KEY}/work_item/create",
            headers={
                "Authorization": f"Bearer {MEEGLE_TOKEN}",
                "Content-Type": "application/json"
            },
            json={
                "name": f"[{cluster}] {tender.get('title', '')}",
                "description": summary,
                "field_values": {
                    "tender_number": tender.get("number"),
                    "customer": cluster,
                    "region": tender.get("region"),
                    "amount": tender.get("amount"),
                    "deadline": tender.get("deadline"),
                    "url": tender.get("url"),
                    "bitable_record": tender.get("record_id")
                },
                "assignees": ["ou_82c8dc631cdd9f0ecc9a074f4a2c6037"],  # Albert
                "due_date": tender.get("deadline")
            },
            timeout=30
        )
        return r.json()
```

### Вариант 3: Meegle Plugin через Lark Event
В Meegle → Project Settings → Apps → Add Lark app → связать с Bitable.

## Workflow

1. **Bitable → Webhook** (нужно настроить Automation в Lark):
   - Trigger: record created
   - Webhook URL: `https://your-dokploy.com/webhook/bitable-new-record`
   - Method: POST

2. **FastAPI** принимает webhook:
   - Парсит запись
   - Создаёт задачу в Meegle
   - Сохраняет `meegle_task_id` в Bitable (новое поле)

3. **Bitable update**:
   - После создания в Meegle → обновить запись с `meegle_task_id`

## Нужные ENV
```bash
MEEGLE_TOKEN=<API token from Meegle>
MEEGLE_PROJECT_KEY=didal-tenders
```

## Нужные scopes в Lark App
- `bitable:automation:read` — для automation
- `bitable:automation:write` — для создания webhook

## Следующие шаги
1. Создать проект в Meegle (если ещё нет)
2. Получить Meegle API token
3. Создать Automation в Lark Bitable (Trigger: record created → webhook)
4. Протестировать end-to-end
