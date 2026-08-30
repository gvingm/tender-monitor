#!/bin/bash
# Deploy tender-monitor to Dokploy
# Usage: DOKPLOY_URL=https://dokploy.example.com DOKPLOY_TOKEN=xxx ./deploy.sh

set -e

DOKPLOY_URL=${DOKPLOY_URL:?"DOKPLOY_URL not set"}
DOKPLOY_TOKEN=${DOKPLOY_TOKEN:?"DOKPLOY_TOKEN not set"}
APP_NAME=${APP_NAME:-tender-monitor}

echo "=== Deploying $APP_NAME to $DOKPLOY_URL ==="

# 1. Create application
APP_RESP=$(curl -s -X POST "$DOKPLOY_URL/api/application.create" \
  -H "Authorization: Bearer $DOKPLOY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "'$APP_NAME'",
    "type": "application"
  }')
APP_ID=$(echo $APP_RESP | python3 -c "import sys, json; print(json.load(sys.stdin).get('applicationId', ''))")
echo "App created: $APP_ID"

# 2. Deploy
curl -s -X POST "$DOKPLOY_URL/api/application.deploy" \
  -H "Authorization: Bearer $DOKPLOY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "applicationId": "'$APP_ID'"
  }'

echo "Deploy triggered. Check $DOKPLOY_URL for status."