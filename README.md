# Tender Monitor — FastAPI

Мониторинг тендеров 44-ФЗ/223-ФЗ с автозаписью в Lark Bitable и уведомлениями.

## Стек
- **Backend:** FastAPI :8787
- **Scheduler:** APScheduler (cron 8:00 MSK daily)
- **Storage:** Lark Bitable (Тендеры Дидал-СК)
- **Notifications:** Lark chat
- **LLM:** Kimi K2.6 (резюме)

## Endpoints
- `GET /` — health check
- `POST /run` — запуск вручную
- `GET /tenders?status=Новый` — список тендеров

## ENV
```bash
TENDERPLAN_KEY=<128 chars>
KIMI_KEY=<kimi api key>
```

Lark credentials захардкожены в `main.py` (для production вынести в ENV).

## Запуск
```bash
pip install -r requirements.txt
python main.py
# или
uvicorn main:app --host 0.0.0.0 --port 8787
```

## Deploy на Dokploy
1. Создать сервис в Dokploy
2. Build context: `/workspace/tender-monitor/`
3. Dockerfile: python:3.13-slim
4. CMD: `uvicorn main:app --host 0.0.0.0 --port 8787`
5. Добавить ENV переменные
