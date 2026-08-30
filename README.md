# Tender Monitor — Go

Мониторинг тендеров 44-ФЗ/223-ФЗ с автозаписью в Lark Bitable и уведомлениями.
**Go-порт** (бывший Python FastAPI). Stdlib only, без зависимостей.

## Стек
- **Backend:** Go 1.22+ (`net/http`) :8787
- **Scheduler:** встроенный goroutine — ежедневный запуск в 08:00 локального TZ
- **Storage:** Lark Bitable (Тендеры Дидал-СК)
- **Notifications:** Lark chat
- **LLM:** Kimi K2.6 (резюме)

## Endpoints
- `GET /` — health check
- `POST /run` — запуск вручную
- `GET /tenders?status=Новый` — список тендеров из Bitable
- `POST /webhook/mail-tenders` — вход от Mail Assistant (HMAC SHA256)

## ENV
```bash
TENDERPLAN_KEY=<128 chars from tenderplan.ru>
KIMI_KEY=<from moonshot.ai>
MAIL_SECRET=<openssl rand -hex 32>
LARK_APP_ID=cli_...
LARK_APP_SECRET=...
LARK_BITABLE_APP=...    # опционально, есть default
LARK_BITABLE_TABLE=...  # опционально, есть default
LARK_CHAT_ID=...        # опционально, есть default
```

## Запуск
```bash
# Сборка
go build -o tender-monitor .

# Запуск
./tender-monitor
# → слушает :8787
```

Или через Docker (multi-stage alpine):
```bash
docker build -t tender-monitor .
docker run --rm -p 8787:8787 --env-file .env tender-monitor
```

## Deploy на Dokploy
1. Создать Application в Dokploy
2. Build context: `/workspace/tender-monitor/`
3. Dockerfile: стандартный (multi-stage Go уже встроен)
4. Port: 8787
5. Добавить ENV из `.env.example`

## Что изменилось vs Python-версия
- Один файл `main.go` (~430 строк), stdlib only — нет `requirements.txt`, нет `pip install`
- Dockerfile — multi-stage Go, итоговый образ ~20 MB (alpine)
- Конфиг через `os.Getenv`, без `.env`-лоадера (переменные передаются окружением)
- Крон-расписание — нативная горутина с `time.Timer` (вместо APScheduler)
- HMAC-проверка — `crypto/hmac` (вместо Python `hmac`)

API и поведение полностью совместимы с Python-версией.
