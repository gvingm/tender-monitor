# Mail Assistant → Bitable парсер

Webhook endpoint для Mail Assistant. Получает тендеры → Bitable + Kimi-резюме.

## Endpoint

```python
POST /webhook/mail-tenders
Content-Type: application/json
X-Mail-Signature: <hmac>

{
  "source": "RosTender",
  "tenders": [
    {
      "id": "...",
      "title": "...",
      "customer": "...",
      "region": "...",
      "amount": 2450000,
      "deadline": "2026-09-15",
      "url": "https://..."
    }
  ]
}
```

## Логика

1. **HMAC verify** — защита от подделки
2. **Дедупликация** — search в Bitable по "Номер"
3. **Kimi-резюме** — генерация
4. **Create record** в Bitable
5. **Notify в группу** — Lark chat

## Код

```python
@app.post("/webhook/mail-tenders")
async def mail_tenders(request: Request):
    body = await request.body()
    signature = request.headers.get("X-Mail-Signature", "")

    # 1. HMAC verify
    if not verify_hmac(body, signature, MAIL_SECRET):
        raise HTTPException(401, "Bad signature")

    # 2. Parse
    data = await request.json()
    tenders = data.get("tenders", [])

    results = {"added": 0, "duplicates": 0, "errors": []}
    for tender in tenders:
        try:
            cluster = classify_cluster(tender)  # Росморпорт/Малый техфлот/Иное
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
```

## ENV
```bash
MAIL_SECRET=<hmac secret from Mail Assistant config>
```

## Настройка Mail Assistant

В Mail Assistant → Settings → Webhooks:
- URL: `https://your-dokploy-domain.com/webhook/mail-tenders`
- Format: JSON
- Secret: сгенерируй `openssl rand -hex 32`
- Events: "New tender found"
