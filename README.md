# Tender Monitor — Go

Мониторинг тендеров 44-ФЗ/223-ФЗ с автозаписью в Lark Bitable и уведомлениями.
**Go-порт** (бывший Python FastAPI). Stdlib only, без зависимостей.

## Стек
- **Backend:** Go 1.22+ (`net/http`) :8787
- **Scheduler:** встроенные goroutine — ежедневный запуск в 08:00 локального TZ + файловый пайплайн каждые 5 минут
- **Storage:** Lark Bitable (Тендеры Дидал-СК)
- **Notifications:** Lark chat
- **LLM:** Kimi K2.6 (резюме, `max_tokens >= 512` — иначе reasoning-модель возвращает пустой ответ)

## Tenderplan API (текущая версия)
- Аутентификация: заголовок `Authorization: Bearer <TENDERPLAN_KEY>` (**не** `?key=`).
- Список: `GET /api/tenders/v2/getlist?q=<слова через OR>&page=0` → `{"tender": <полная модель 1-го>, "tenders": [<короткие модели>]}`. Пагинация: `page=0,1,2...` (максимум 3 страницы на кластер).
- Параметра `since` **нет**; фильтры дат: `fromPublicationDateTime`, `toSubmissionCloseDateTime` и др. (unix мс).
- В короткой модели **нет** `href` — ссылка на площадку-источник догружается полной моделью: `GET /api/tenders/get?id=<_id>` (только для новых тендеров, после проверки дублей).
- Файлы: `GET /api/tenders/attachments?id=<_id>` → массив `{"displayName","href","realName","size","publicationDateTime"}`; `href` — прямая ссылка на файл на площадке-источнике. Запасной прокси: `GET /api/tenders/file?href=<urlencoded>` (deprecated, но работает).
- Лимиты: 100 запросов/10с, 500/мин (60/мин на getlist) — между запросами паузы 250 мс, один `http.Client` с keep-alive.

## Маппинг полей Tenderplan → Tender
| Tenderplan (короткая модель) | Tender |
|---|---|
| `_id` | `ID` (пишется в Bitable как `TenderplanID`) |
| `number` | `Number` |
| `orderName` | `Name` |
| `customers[0].name` | `Customer` |
| `region` (числовой код) | `Region` (строкой) |
| `maxPrice` | `Amount` |
| `submissionCloseDateTime` (мс) | `Deadline` (RFC3339) |
| `href` (из полной модели) | `URL` |

## Файловый пайплайн (новое)
При переводе карточки в статус **«На рассмотрении»** сервис скачивает файлы тендера в карточку:

1. Поиск в Bitable: `Статус = "На рассмотрении"` AND `ФайлыЗагружены = false`
   (если поле `ФайлыЗагружены` отсутствует — fallback: `Статус` + пустое поле `Файлы`).
2. По `TenderplanID` → `GET /api/tenders/attachments` → список файлов.
3. Каждый файл скачивается по `href` (таймаут 60 с/файл; при 401/403/404 — прокси `/api/tenders/file`),
   потоково во временный файл в `os.TempDir()` (не в память), имя — `realName` (санитизируется).
4. Загрузка в карточку: `POST /open-apis/drive/v1/medias/upload_all` (`parent_type=bitable_file`,
   `parent_node=<bitableApp>`) → `file_token`; затем одно обновление записи `PUT .../records/{id}`
   с полем `Файлы: [{"file_token": ...}]` и `ФайлыЗагружены = true`.
5. Каждый файл логируется (имя, размер, результат). Временные файлы удаляются после загрузки.

Триггеры:
- периодический опрос каждые **5 минут** (goroutine с ticker);
- ручной запуск: `POST /files/run` → JSON со счётчиками.

Если полей `TenderplanID` (Text), `Файлы` (Attachment), `ФайлыЗагружены` (Checkbox) нет в таблице —
сервис создаёт их через `POST .../tables/{table}/fields` при первой ошибке «поле не существует»;
если создать не удалось — поле просто пропускается.

## Endpoints
- `GET /` — health check
- `POST /run` — запуск мониторинга вручную
- `POST /files/run` — ручной запуск файлового пайплайна
- `GET /tenders?status=Новый` — список тендеров из Bitable
- `POST /webhook/mail-tenders` — вход от Mail Assistant (HMAC SHA256)

## ENV
```bash
TENDERPLAN_KEY=<128 chars from tenderplan.ru>   # fallback: Tenderplan_API_Key
KIMI_KEY=<from moonshot.ai>                      # fallback: Kimi_API_Key
MAIL_SECRET=<openssl rand -hex 32>
LARK_APP_ID=cli_...                              # fallback: Lark_App_ID
LARK_APP_SECRET=...                              # fallback: Lark_App_Secret
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
- Один файл `main.go`, stdlib only — нет `requirements.txt`, нет `pip install`
- Dockerfile — multi-stage Go, итоговый образ ~20 MB (alpine)
- Конфиг через `os.Getenv`, без `.env`-лоадера (переменные передаются окружением)
- Крон-расписание — нативная горутина с `time.Timer` (вместо APScheduler)
- HMAC-проверка — `crypto/hmac` (вместо Python `hmac`)
- Переход на реальный Tenderplan API: Bearer-заголовок, `v2/getlist`, пагинация, догрузка `href`
- Файловый пайплайн: скачивание вложений тендера в карточку Bitable по статусу «На рассмотрении»
