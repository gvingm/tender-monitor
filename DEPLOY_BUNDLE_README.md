# Tender Monitor — Deploy Bundle

## What's here

- main.py — FastAPI app
- Dockerfile — Python 3.13-slim, port 8787
- equirements.txt
- .env — filled with current values (TENDERPLAN_KEY, KIMI_KEY, MAIL_SECRET)
- .env.example — template without secrets
- deploy.sh / deploy.ps1 — Dokploy deploy scripts
- README.md, DOKPLOY.md, MAIL_INTEGRATION.md, MEEGLE_INTEGRATION.md

## Local run (verified working)

`ash
docker build -t tender-monitor .
docker run -p 8787:8787 --env-file .env tender-monitor
`

Or directly:
`ash
pip install -r requirements.txt
uvicorn main:app --host 0.0.0.0 --port 8787
`

## Deploy to Dokploy

### Option A: One command
`ash
 = "https://dokploy.example.com"
 = "your-token"
.\deploy.ps1
`

### Option B: Manual via Dokploy UI
1. Create Application 	ender-monitor (Docker)
2. Build context: this folder
3. Dockerfile: ./Dockerfile
4. Port: 8787
5. ENV: TENDERPLAN_KEY, KIMI_KEY, MAIL_SECRET
6. Deploy

## Endpoints
- GET / — health
- POST /run — manual run
- GET /tenders?status=Новый — list
- POST /webhook/mail-tenders — Mail Assistant webhook (HMAC verified)