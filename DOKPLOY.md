# Dokploy Deployment Config

## Service
- **Name:** tender-monitor
- **Type:** Application (Docker)
- **Repository:** local (or GitHub)
- **Branch:** main
- **Build Path:** `/workspace/tender-monitor/`
- **Dockerfile:** `./Dockerfile`

## Port
- **Container:** 8787
- **Host:** auto (or 8787)

## Environment Variables
```
KIMI_KEY=<from .env>
TENDERPLAN_KEY=<from .env>
MAIL_SECRET=<generate openssl rand -hex 32>
```

## Domain
После деплоя Dokploy выдаст URL вида:
`https://tender-monitor-xxxx.dokploy.app`

## Health Check
- Path: `/`
- Expected: `{"status":"ok",...}`

## Volumes (опционально)
Для логов:
- Host: `/var/log/tender-monitor`
- Container: `/app/logs`

## После деплоя
1. Скопируй URL (например `https://tender-monitor-xxx.dokploy.app`)
2. Используй для:
   - Mail Assistant webhook: `https://...dokploy.app/webhook/mail-tenders`
   - Lark Event Subscription: `https://...dokploy.app/lark/webhook`
   - Telegram polling (если добавим)

## Проверка
```bash
curl https://tender-monitor-xxx.dokploy.app/
# {"status":"ok","service":"tender-monitor"}
```

## Автозапуск cron
- APScheduler в `main.py` запускает `process_tenders()` в 8:00 MSK
- Не нужно настраивать отдельно
