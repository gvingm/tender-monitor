// Package main — Tender Monitor: мониторинг тендеров Tenderplan + Lark Bitable + Kimi-резюме
// + скачивание файлов тендера (статус «На рассмотрении» / воронка «интересные»);
// интересная карточка переходит в воронку «Документы скачал» только после
// успешной загрузки ВСЕХ файлов (частичная загрузка не фиксируется — повтор в следующем проходе).
//
// Go-порт исходного Python FastAPI-приложения (см. git history).
// Один файл, stdlib only.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ===== CONFIG =====

const (
	tenderplanAPI       = "https://tenderplan.ru/api"
	kimiAPI             = "https://api.moonshot.ai/v1/chat/completions"
	kimiFilesAPI        = "https://api.moonshot.ai/v1/files" // Kimi Files API (загрузка ТЗ для анализа)
	kimiModel           = "kimi-k2.6"
	larkBase            = "https://open.larksuite.com/open-apis"
	listenAddr          = "0.0.0.0:8787" // IPv4 явно: WSL localhostForwarding не пробрасывает tcp6-only сокеты
	dailyRunHour        = 8
	dailyRunMinute      = 0
	filesPollInterval   = 5 * time.Minute  // периодический опрос карточек «На рассмотрении»
	marksPollInterval   = 5 * time.Minute  // периодический опрос меток Tenderplan (воронка «интересные»)
	marksMaxPages       = 10               // cap страниц v1 getlist при опросе меток (50/стр.)
	fileDownloadTimeout = 60 * time.Second // таймаут на скачивание одного файла
	maxAnalysisFileSize = 50 << 20         // файлы >50 МБ в анализ ТЗ не берём
	analysisRetryDelay  = time.Hour        // минимальный интервал между попытками анализа ТЗ одной записи
	maxListPages        = 1                // страниц getlist на ключевое слово (50/стр.; дневному дайджесту хватает)
	maxNewPerRun        = 20               // максимум новых тендеров за прогон (Kimi дорогой, чат не спамим)
	tpRequestPause      = 250 * time.Millisecond
	tokenTTL            = 2 * time.Hour

	defaultAppID        = "cli_aa1810fe6f78c079"
	defaultBitableApp   = "X37KbBltZaqSSdsGyJdumNLFtmh"
	defaultBitableTable = "tblalXw0gAIi3pVc"
	defaultChatID       = "oc_6cc3a4c2e69b74e6a7d240c1e95db951"
)

var (
	// Поддерживаем и старые mixed-case имена переменных (fallback).
	tenderplanKey = envOrMulti("", "TENDERPLAN_KEY", "Tenderplan_API_Key")
	kimiKey       = envOrMulti("", "KIMI_KEY", "Kimi_API_Key")
	mailSecret    = os.Getenv("MAIL_SECRET")
	larkAppID     = envOrMulti(defaultAppID, "LARK_APP_ID", "Lark_App_ID")
	larkAppSecret = envOrMulti("", "LARK_APP_SECRET", "Lark_App_Secret")
	bitableApp    = envOrMulti(defaultBitableApp, "LARK_BITABLE_APP", "Lark_Bitable_App")
	bitableTable  = envOrMulti(defaultBitableTable, "LARK_BITABLE_TABLE", "Lark_Bitable_Table")
	chatID        = envOrMulti(defaultChatID, "LARK_CHAT_ID", "Lark_Chat_ID")
)

func envOrMulti(def string, keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return def
}

var clusters = map[string][]string{
	"Росморпорт":    {"Росморпорт", "морской порт", "акватория", "дноуглубление", "берегоукрепление", "гидротехника", "причал"},
	"Малый техфлот": {"земснаряд", "буксир", "шаланда", "понтон", "катер", "моторная яхта", "маломерное судно"},
	"Иное":          {"дноуглубление", "дноуглубительные работы", "судостроение", "гидротехника", "аренда флота"},
}

// moscowTZ — локальная тайзона для подписей воронок; если в контейнере нет tzdata —
// фиксированный UTC+3 (Europe/Moscow без исторических переходов).
var moscowTZ = func() *time.Location {
	if l, err := time.LoadLocation("Europe/Moscow"); err == nil {
		return l
	}
	return time.FixedZone("MSK", 3*60*60)
}()

// funnelDocsDownloaded — значение поля «Воронка» для интересных тендеров,
// у которых ВСЕ документы успешно скачаны и загружены в карточку.
const funnelDocsDownloaded = "Документы скачал"

// voronkaNewLabel — значение поля «Воронка» для новых тендеров текущего прогона.
func voronkaNewLabel() string {
	return "новые от " + time.Now().In(moscowTZ).Format("02.01.2006")
}

// Регулярные выражения новой логики (воронки / метки / анализ ТЗ).
var (
	// interestingMarkRe — имя метки Tenderplan «Интересно» (и производные).
	interestingMarkRe = regexp.MustCompile(`(?i)интерес`)
	// dredgeRe — признак дноуглубительного тендера в названии.
	dredgeRe = regexp.MustCompile(`(?i)(дноуглуб|расчистк[аи]\s+русл|углублени[ея]\s+(дна|русл|акватор))`)
	// tzFileRe — признак файла ТЗ в имени вложения.
	tzFileRe = regexp.MustCompile(`(?i)(тз|техническ|задание)`)
)

// ===== HTTP CLIENTS =====
// Один общий клиент с keep-alive для всех API; файлы — отдельный клиент без
// общего таймаута (таймаут задаётся контекстом на каждый файл).

var apiClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	},
}

var fileClient = &http.Client{
	// без общего Timeout: ограничение 60с ставим через context на каждый файл
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
	},
}

// kimiClient — отдельный клиент с длинным таймаутом: kimi-k2.6 — reasoning-модель,
// генерация с «размышлениями» может занимать 60-120 секунд.
var kimiClient = &http.Client{
	Timeout: 180 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	},
}

// ===== LARK AUTH =====

type tokenCache struct {
	mu    sync.Mutex
	value string
	exp   time.Time
}

var tkCache = &tokenCache{}

func getTenantToken(ctx context.Context) (string, error) {
	tkCache.mu.Lock()
	if tkCache.value != "" && time.Now().Before(tkCache.exp) {
		v := tkCache.value
		tkCache.mu.Unlock()
		return v, nil
	}
	tkCache.mu.Unlock()

	req, _ := http.NewRequestWithContext(ctx, "POST",
		larkBase+"/auth/v3/tenant_access_token/internal",
		bytes.NewBufferString(fmt.Sprintf(`{"app_id":"%s","app_secret":"%s"}`, larkAppID, larkAppSecret)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := apiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.TenantAccessToken == "" {
		return "", fmt.Errorf("lark: пустой tenant_access_token")
	}
	tkCache.mu.Lock()
	tkCache.value = out.TenantAccessToken
	tkCache.exp = time.Now().Add(tokenTTL)
	tkCache.mu.Unlock()
	return out.TenantAccessToken, nil
}

// ===== TENDERPLAN =====

// Tender — внутренняя модель тендера (и формат входа webhook mail-tenders).
type Tender struct {
	ID          string  `json:"id"` // TenderplanID (_id)
	Number      string  `json:"number"`
	Name        string  `json:"name"`
	Customer    string  `json:"customer"`
	Region      string  `json:"region"`
	Amount      float64 `json:"amount"`
	Deadline    string  `json:"deadline"` // RFC3339
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
}

// Короткая модель из /api/tenders/v2/getlist.
type tpShortTender struct {
	ID                      string          `json:"_id"`
	Number                  string          `json:"number"`
	OrderName               string          `json:"orderName"`
	MaxPrice                *float64        `json:"maxPrice"`
	Currency                string          `json:"currency"`
	Region                  json.RawMessage `json:"region"` // числовой код (напр. 39), на всякий случай RawMessage
	PublicationDateTime     int64           `json:"publicationDateTime"`
	SubmissionCloseDateTime int64           `json:"submissionCloseDateTime"`
	PlacingWay              int             `json:"placingWay"`
	Kind                    int             `json:"kind"`
	Status                  int             `json:"status"`
	Customers               []struct {
		GUID   string `json:"guid"`
		Name   string `json:"name"`
		Region int    `json:"region"`
	} `json:"customers"`
	Href string `json:"href"` // в короткой модели обычно отсутствует
}

type tpListResponse struct {
	// "tender" — полная модель первого тендера выборки (можно взять href без лишнего запроса).
	Tender *struct {
		ID   string `json:"_id"`
		Href string `json:"href"`
	} `json:"tender"`
	Tenders []tpShortTender `json:"tenders"`
}

// rawToString конвертирует RawMessage (число или строка) в строку.
func rawToString(r json.RawMessage) string {
	s := strings.TrimSpace(string(r))
	if s == "" || s == "null" {
		return ""
	}
	s = strings.Trim(s, `"`)
	if f, err := strconv.ParseFloat(s, 64); err == nil && f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return s
}

// tpGet выполняет GET к tenderplan API с Bearer-авторизацией и паузой после запроса (лимиты).
func tpGet(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tenderplanKey)
	resp, err := apiClient.Do(req)
	time.Sleep(tpRequestPause)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tenderplan HTTP %d: %.200s", resp.StatusCode, string(body))
	}
	return json.Unmarshal(body, out)
}

// fetchTenders — выборка тендеров кластера через v2/getlist с пагинацией (page=0,1,2...).
// since (RFC3339, опционально) маппится в fromPublicationDateTime (мс).
//
// ВАЖНО: Tenderplan НЕ поддерживает синтаксис "OR" в q — OR-запрос возвращает 0
// (проверено эмпирически 2026-08-31). Поэтому опрашиваем КАЖДОЕ ключевое слово
// отдельно и склеиваем результат с дедупликацией по _id.
func fetchTenders(ctx context.Context, clusterName string, keywords []string, since string) ([]Tender, error) {
	var all []Tender
	seen := map[string]bool{}
	var errs []string
	for _, kw := range keywords {
		for page := 0; page < maxListPages; page++ {
			u := fmt.Sprintf("%s/tenders/v2/getlist?q=%s&page=%d",
				tenderplanAPI, url.QueryEscape(kw), page)
			if since != "" {
				if ts, err := time.Parse(time.RFC3339, since); err == nil {
					u += "&fromPublicationDateTime=" + strconv.FormatInt(ts.UnixMilli(), 10)
				}
			}
			var out tpListResponse
			if err := tpGet(ctx, u, &out); err != nil {
				errs = append(errs, fmt.Sprintf("%s[%s] page=%d: %v", clusterName, kw, page, err))
				break
			}
			if len(out.Tenders) == 0 {
				break
			}
			for i, st := range out.Tenders {
				if st.ID == "" || seen[st.ID] {
					continue
				}
				seen[st.ID] = true
				t := Tender{
					ID:       st.ID,
					Number:   st.Number,
					Name:     st.OrderName,
					Region:   rawToString(st.Region),
					Title:    st.OrderName,
					Deadline: msToRFC3339(st.SubmissionCloseDateTime),
				}
				if st.MaxPrice != nil {
					t.Amount = *st.MaxPrice
				}
				if len(st.Customers) > 0 {
					t.Customer = st.Customers[0].Name
					if t.Region == "" {
						t.Region = strconv.Itoa(st.Customers[0].Region)
					}
				}
				// href: из короткой модели, либо из полной модели первого элемента выборки.
				t.URL = st.Href
				if t.URL == "" && page == 0 && i == 0 && out.Tender != nil && out.Tender.ID == st.ID {
					t.URL = out.Tender.Href
				}
				all = append(all, t)
			}
		}
	}
	if len(all) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("%s: %s", clusterName, strings.Join(errs, "; "))
	}
	return all, nil
}

// tpFullTender — полная модель тендера (GET /api/tenders/get?id=<_id>): нужные нам поля.
type tpFullTender struct {
	Href              string `json:"href"`
	OrderName         string `json:"orderName"`
	Description       string `json:"description"`
	NoticeDescription string `json:"noticeDescription"`
}

// fetchTenderDetails догружает полную модель тендера (ссылка, название, описание).
// Некоторые версии API заворачивают модель в {"tender": {...}} — поддерживаем оба варианта.
func fetchTenderDetails(ctx context.Context, id string) (tpFullTender, error) {
	var raw json.RawMessage
	if err := tpGet(ctx, tenderplanAPI+"/tenders/get?id="+url.QueryEscape(id), &raw); err != nil {
		return tpFullTender{}, err
	}
	var direct tpFullTender
	if err := json.Unmarshal(raw, &direct); err == nil && (direct.Href != "" || direct.OrderName != "") {
		return direct, nil
	}
	var wrap struct {
		Tender *tpFullTender `json:"tender"`
	}
	if err := json.Unmarshal(raw, &wrap); err == nil && wrap.Tender != nil {
		return *wrap.Tender, nil
	}
	return direct, nil
}

// fetchTenderHref догружает полную модель тендера ради ссылки на площадку-источник.
func fetchTenderHref(ctx context.Context, id string) (string, error) {
	d, err := fetchTenderDetails(ctx, id)
	if err != nil {
		return "", err
	}
	return d.Href, nil
}

func msToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Format(time.RFC3339)
}

// ===== KIMI =====

func kimiSummarize(ctx context.Context, t Tender) (string, error) {
	prompt := fmt.Sprintf(`Сделай краткое резюме (5-7 строк) этого тендера:

Название: %s
Заказчик: %s
Описание: %s
Сумма: %s ₽
Регион: %s
Срок подачи: %s

Формат:
[Суть закупки одним предложением]
Заказчик: [тип и регион]
Сумма: [₽]
Срок: [дата]
Ключевые требования: [3-4 пункта]
Наша оценка шансов: [высокие/средние/низкие]
Рекомендация: [подавать/не подавать]`,
		t.Name, t.Customer, t.Description, formatAmount(t.Amount), t.Region, t.Deadline)

	// ВАЖНО: reasoning-модель съедает токены на «размышления» — нужен запас,
	// иначе content приходит пустым (finish_reason=length). И длинный таймаут.
	// temperature НЕ передаём: kimi-k2.6 принимает только temperature=1,
	// другое значение → HTTP 400 invalid_request_error.
	body, _ := json.Marshal(map[string]any{
		"model":      kimiModel,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": 4096,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", kimiAPI, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+kimiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := kimiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("kimi: empty response")
	}
	return out.Choices[0].Message.Content, nil
}

func formatAmount(a float64) string {
	return strconv.FormatFloat(a, 'f', 0, 64)
}

// ===== LARK BITABLE =====

type bitableResult struct {
	Status string `json:"status"`
	Record any    `json:"record"`
	Code   int    `json:"code"`
}

// bitableRecord — запись из records/search.
type bitableRecord struct {
	RecordID string         `json:"record_id"`
	Fields   map[string]any `json:"fields"`
}

type larkError struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func (e *larkError) Error() string { return fmt.Sprintf("lark code=%d: %s", e.Code, e.Msg) }

// isFieldNotFound определяет ошибку «поле не существует» в ответе Lark Bitable.
func isFieldNotFound(code int, msg string) bool {
	switch code {
	case 1254043, 1254045, 1254607: // FieldNameNotFound и смежные коды Bitable
		return true
	}
	m := strings.ToLower(msg)
	if strings.Contains(msg, "FieldNameNotFound") {
		return true
	}
	return strings.Contains(m, "field") &&
		(strings.Contains(m, "not found") || strings.Contains(m, "not exist") ||
			strings.Contains(m, "не найден") || strings.Contains(m, "не существует"))
}

// larkJSON — POST/PUT к Lark Open API с tenant token; возвращает тело ответа.
func larkJSON(ctx context.Context, method, u string, payload any) ([]byte, error) {
	token, err := getTenantToken(ctx)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// bitableSearch ищет записи с фильтром (conjunction/conditions), с пагинацией.
// Возвращает записи или *larkError.
func bitableSearch(ctx context.Context, conditions []map[string]any, fieldNames []string) ([]bitableRecord, error) {
	u := fmt.Sprintf("%s/bitable/v1/apps/%s/tables/%s/records/search", larkBase, bitableApp, bitableTable)
	var records []bitableRecord
	pageToken := ""
	for pages := 0; pages < 5; pages++ { // cap: 5 страниц × 100 записей
		payload := map[string]any{
			"page_size": 100,
			"filter": map[string]any{
				"conjunction": "and",
				"conditions":  conditions,
			},
		}
		if len(fieldNames) > 0 {
			payload["field_names"] = fieldNames
		}
		if pageToken != "" {
			payload["page_token"] = pageToken
		}
		body, err := larkJSON(ctx, "POST", u, payload)
		if err != nil {
			return nil, err
		}
		var out struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				HasMore   bool            `json:"has_more"`
				PageToken string          `json:"page_token"`
				Items     []bitableRecord `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		if out.Code != 0 {
			return nil, &larkError{Code: out.Code, Msg: out.Msg}
		}
		records = append(records, out.Data.Items...)
		if !out.Data.HasMore {
			break
		}
		pageToken = out.Data.PageToken
	}
	return records, nil
}

// findRecordByNumber — проверка дубля по полю «Номер».
func findRecordByNumber(ctx context.Context, number string) (bool, error) {
	recs, err := bitableSearch(ctx, []map[string]any{
		{"field_name": "Номер", "operator": "is", "value": []string{number}},
	}, nil)
	if err != nil {
		return false, err
	}
	return len(recs) > 0, nil
}

// ensureBitableField создаёт поле в таблице (Text=1, Attachment=17, Checkbox=7).
// Ошибки (в т.ч. «поле уже существует») логируются, но не считаются фатальными.
func ensureBitableField(ctx context.Context, fieldName string, fieldType int) error {
	u := fmt.Sprintf("%s/bitable/v1/apps/%s/tables/%s/fields", larkBase, bitableApp, bitableTable)
	body, err := larkJSON(ctx, "POST", u, map[string]any{
		"field_name": fieldName,
		"type":       fieldType,
	})
	if err != nil {
		log.Printf("[bitable] create field %q (type=%d): %v", fieldName, fieldType, err)
		return err
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(body, &out)
	if out.Code != 0 {
		log.Printf("[bitable] create field %q (type=%d): code=%d %s", fieldName, fieldType, out.Code, out.Msg)
		return &larkError{Code: out.Code, Msg: out.Msg}
	}
	log.Printf("[bitable] поле %q (type=%d) создано", fieldName, fieldType)
	return nil
}

// addToBitable: проверка дублей + создание записи. Поле «Заказчик» = кластер (как раньше).
// voronka (если не пусто) пишется в поле «Воронка» («новые от ДД.ММ.ГГГГ» / «интересные»).
// TenderplanID и «Воронка» пишутся для последующей обработки; при ошибке «поле не существует»
// поле создаётся, а если и это не удалось — запись создаётся без него.
func addToBitable(ctx context.Context, t Tender, cluster, summary, voronka string) (bitableResult, error) {
	deadline := any(nil)
	if t.Deadline != "" {
		if ts, err := time.Parse(time.RFC3339, t.Deadline); err == nil {
			deadline = ts.UnixMilli()
		}
	}
	fields := map[string]any{
		"Номер":       t.Number,
		"Заказчик":    cluster,
		"Регион":      t.Region,
		"Сумма":       t.Amount,
		"Срок подачи": deadline,
		"Статус":      "Новый",
		"Ссылка":      map[string]string{"text": t.URL, "link": t.URL},
		"Kimi резюме": summary,
	}
	if t.ID != "" {
		fields["TenderplanID"] = t.ID
	}
	if voronka != "" {
		fields["Воронка"] = voronka
	}

	// duplicate check
	dup, err := findRecordByNumber(ctx, t.Number)
	if err != nil {
		return bitableResult{}, err
	}
	if dup {
		return bitableResult{Status: "duplicate"}, nil
	}

	// create (с одной попыткой самолечения при отсутствии поля)
	code, rec, err := createBitableRecord(ctx, fields)
	if err != nil {
		var le *larkError
		if ok := asLarkError(err, &le); ok && isFieldNotFound(le.Code, le.Msg) {
			log.Printf("[bitable] поле не найдено (%s), пробуем создать и повторить", le.Msg)
			_ = ensureBitableField(ctx, "TenderplanID", 1)
			if voronka != "" {
				_ = ensureBitableField(ctx, "Воронка", 1)
			}
			code, rec, err = createBitableRecord(ctx, fields)
			if err != nil {
				// не падаем: повторяем без спорных полей
				if ok := asLarkError(err, &le); ok && isFieldNotFound(le.Code, le.Msg) {
					delete(fields, "TenderplanID")
					delete(fields, "Воронка")
					code, rec, err = createBitableRecord(ctx, fields)
				}
			}
		}
	}
	if err != nil {
		return bitableResult{}, err
	}
	return bitableResult{Code: code, Record: rec}, nil
}

// recordIDFromCreate извлекает record_id из ответа createBitableRecord.
func recordIDFromCreate(rec any) string {
	m, ok := rec.(map[string]any)
	if !ok {
		return ""
	}
	data, _ := m["data"].(map[string]any)
	r, _ := data["record"].(map[string]any)
	id, _ := r["record_id"].(string)
	return id
}

func asLarkError(err error, out **larkError) bool {
	if le, ok := err.(*larkError); ok {
		*out = le
		return true
	}
	return false
}

func createBitableRecord(ctx context.Context, fields map[string]any) (int, any, error) {
	u := fmt.Sprintf("%s/bitable/v1/apps/%s/tables/%s/records", larkBase, bitableApp, bitableTable)
	body, err := larkJSON(ctx, "POST", u, map[string]any{"fields": fields})
	if err != nil {
		return 0, nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	code := 0
	if c, ok := out["code"].(float64); ok {
		code = int(c)
	}
	if code != 0 {
		msg, _ := out["msg"].(string)
		return code, out, &larkError{Code: code, Msg: msg}
	}
	return code, out, nil
}

// bitableText достаёт строку из текстового поля Bitable
// (в search текст приходит либо строкой, либо массивом сегментов [{text,type}]).
func bitableText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, seg := range t {
			if m, ok := seg.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// ===== LARK NOTIFICATION =====

func notifyLark(ctx context.Context, t Tender, cluster, summary string) error {
	token, err := getTenantToken(ctx)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("🚢 Новый тендер «%s»\n\n💰 Сумма: %s ₽\n📍 Регион: %s\n📅 Срок: %s\n🔗 %s\n\n%s\n\n#тендер #%s",
		cluster, formatAmount(t.Amount), t.Region, t.Deadline, t.URL, summary, strings.ReplaceAll(strings.ToLower(cluster), " ", "_"))
	content, _ := json.Marshal(map[string]string{"text": text})
	body, _ := json.Marshal(map[string]any{
		"receive_id": chatID,
		"msg_type":   "text",
		"content":    string(content),
	})
	u := larkBase + "/im/v1/messages?receive_id_type=chat_id"
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := apiClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ===== CLUSTERING =====

func classifyCluster(t Tender) string {
	text := strings.ToLower(t.Title + " " + t.Description)
	for name, kws := range clusters {
		for _, kw := range kws {
			if strings.Contains(text, strings.ToLower(kw)) {
				return name
			}
		}
	}
	return "Иное"
}

// ===== MAIN WORKFLOW =====

type runResults struct {
	Processed  int      `json:"processed"`
	Added      int      `json:"added"`
	Duplicates int      `json:"duplicates"`
	Errors     []string `json:"errors"`
}

func processTenders(ctx context.Context) runResults {
	res := runResults{}
	voronka := voronkaNewLabel() // воронка для новых записей этого прогона (дата по Europe/Moscow)
	for name, kws := range clusters {
		tenders, err := fetchTenders(ctx, name, kws, "")
		if err != nil {
			res.Errors = append(res.Errors, name+": "+err.Error())
			continue
		}
		for _, t := range tenders {
			// дешёвая пред-проверка дубля до дорогих вызовов (полная модель, Kimi)
			dup, err := findRecordByNumber(ctx, t.Number)
			if err != nil {
				res.Errors = append(res.Errors, t.Number+": dupcheck "+err.Error())
				continue
			}
			if dup {
				res.Processed++
				res.Duplicates++
				continue
			}
			// ссылку на площадку-источник берём из полной модели, если её нет в короткой
			if t.URL == "" && t.ID != "" {
				if href, err := fetchTenderHref(ctx, t.ID); err == nil {
					t.URL = href
				} else {
					log.Printf("[tenderplan] href %s: %v", t.ID, err)
				}
			}
			// Kimi — опционально: при ошибке/лимите API тендер НЕ теряем,
			// пишем в Bitable без резюме (пометка в поле).
			summary, err := kimiSummarize(ctx, t)
			if err != nil {
				log.Printf("[run] kimi %s: %v", t.Number, err)
				res.Errors = append(res.Errors, t.Number+": kimi "+err.Error())
				summary = "⚠️ Резюме недоступно: ошибка Kimi API (см. логи). Данные тендера — в полях карточки."
			}
			r, err := addToBitable(ctx, t, name, summary, voronka)
			if err != nil {
				res.Errors = append(res.Errors, t.Number+": bitable "+err.Error())
				continue
			}
			res.Processed++
			if r.Status == "duplicate" {
				res.Duplicates++
			} else {
				res.Added++
				if err := notifyLark(ctx, t, name, summary); err != nil {
					res.Errors = append(res.Errors, t.Number+": notify "+err.Error())
				}
				if res.Added >= maxNewPerRun {
					log.Printf("[run] достигнут лимит %d новых за прогон, остаток — в следующие запуски", maxNewPerRun)
					return res
				}
			}
		}
	}
	return res
}

func processMailTenders(ctx context.Context, tenders []Tender) runResults {
	res := runResults{}
	for _, t := range tenders {
		cluster := classifyCluster(t)
		// Kimi — опционально: при ошибке/лимите API тендер не теряем
		summary, err := kimiSummarize(ctx, t)
		if err != nil {
			log.Printf("[mail] kimi %s: %v", t.Number, err)
			res.Errors = append(res.Errors, t.Number+": kimi "+err.Error())
			summary = "⚠️ Резюме недоступно: ошибка Kimi API (см. логи). Данные тендера — в полях карточки."
		}
		r, err := addToBitable(ctx, t, cluster, summary, "")
		if err != nil {
			res.Errors = append(res.Errors, t.Number+": bitable "+err.Error())
			continue
		}
		res.Processed++
		if r.Status == "duplicate" {
			res.Duplicates++
		} else {
			res.Added++
			if err := notifyLark(ctx, t, cluster, summary); err != nil {
				res.Errors = append(res.Errors, t.Number+": notify "+err.Error())
			}
		}
	}
	return res
}

// ===== FILE DOWNLOAD PIPELINE =====
// Скачивание файлов тендера в карточку Bitable при переводе её в статус «На рассмотрении».

// tpAttachment — элемент ответа GET /api/tenders/attachments?id=<_id>.
type tpAttachment struct {
	DisplayName         string `json:"displayName"`
	Href                string `json:"href"` // прямая ссылка на файл на площадке-источнике
	RealName            string `json:"realName"`
	Size                int64  `json:"size"`
	PublicationDateTime int64  `json:"publicationDateTime"`
}

type fileRunResult struct {
	StartedAt       string   `json:"started_at"`
	FinishedAt      string   `json:"finished_at"`
	RecordsFound    int      `json:"records_found"`
	RecordsDone     int      `json:"records_done"`
	FilesDownloaded int      `json:"files_downloaded"`
	FilesUploaded   int      `json:"files_uploaded"`
	FilesFailed     int      `json:"files_failed"`
	TZAnalyses      int      `json:"tz_analyses"` // выполненных анализов ТЗ (дноуглубительные из «интересные»)
	Errors          []string `json:"errors,omitempty"`
}

var filesRunMu sync.Mutex // защита от параллельных запусков файлового пайплайна

// fetchAttachments — список файлов тендера по TenderplanID.
func fetchAttachments(ctx context.Context, tenderID string) ([]tpAttachment, error) {
	u := tenderplanAPI + "/tenders/attachments?id=" + url.QueryEscape(tenderID)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tenderplanKey)
	resp, err := apiClient.Do(req)
	time.Sleep(tpRequestPause)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("attachments HTTP %d: %.200s", resp.StatusCode, string(body))
	}
	// ответ — массив; на всякий случай поддерживаем обёртки {"data":[...]} / {"attachments":[...]}
	var arr []tpAttachment
	if err := json.Unmarshal(body, &arr); err == nil {
		return arr, nil
	}
	var wrap map[string]json.RawMessage
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, err
	}
	for _, k := range []string{"data", "attachments", "items"} {
		if raw, ok := wrap[k]; ok {
			if err := json.Unmarshal(raw, &arr); err == nil {
				return arr, nil
			}
		}
	}
	return nil, fmt.Errorf("attachments: неизвестный формат ответа: %.200s", string(body))
}

// sanitizeFileName чистит имя файла от недопустимых символов (Windows-safe).
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	repl := strings.NewReplacer(
		"<", "_", ">", "_", ":", "_", `"`, "_", "/", "_", `\`, "_", "|", "_", "?", "_", "*", "_",
	)
	name = repl.Replace(name)
	name = strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" {
		name = "file"
	}
	if len([]rune(name)) > 120 {
		r := []rune(name)
		ext := ""
		if i := strings.LastIndex(name, "."); i > 0 && len(name)-i <= 10 {
			ext = name[i:]
		}
		name = string(r[:120-len([]rune(ext))]) + ext
	}
	return name
}

// downloadAttachment скачивает файл потоково во временный файл в os.TempDir().
// Прямая ссылка; при 401/403/404 — запасной прокси /api/tenders/file?href= (deprecated, но работает).
// Возвращает путь к временному файлу (вызывающий обязан удалить) и размер.
func downloadAttachment(ctx context.Context, att tpAttachment) (string, int64, error) {
	status, path, size, err := downloadToTemp(ctx, att.Href, false)
	if err == nil {
		return path, size, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound {
		log.Printf("[files] direct %q -> HTTP %d, пробуем прокси /api/tenders/file", att.RealName, status)
		proxyURL := tenderplanAPI + "/tenders/file?href=" + url.QueryEscape(att.Href)
		_, path, size, err = downloadToTemp(ctx, proxyURL, true)
	}
	if err != nil {
		return "", 0, err
	}
	return path, size, nil
}

// downloadToTemp качает URL во временный файл; bearer=true добавляет Authorization.
func downloadToTemp(ctx context.Context, u string, bearer bool) (int, string, int64, error) {
	fctx, cancel := context.WithTimeout(ctx, fileDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, "GET", u, nil)
	if err != nil {
		return 0, "", 0, err
	}
	if bearer {
		req.Header.Set("Authorization", "Bearer "+tenderplanKey)
	}
	resp, err := fileClient.Do(req)
	if err != nil {
		return 0, "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, "", 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(os.TempDir(), "tender-*.bin")
	if err != nil {
		return 0, "", 0, err
	}
	size, err := io.Copy(tmp, resp.Body)
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		return 0, "", 0, err
	}
	return resp.StatusCode, tmp.Name(), size, nil
}

// uploadFileToDrive загружает локальный файл в Lark Drive (bitable_file) и возвращает file_token.
// Потоковая multipart-отправка через io.Pipe — файл не читается в память целиком.
func uploadFileToDrive(ctx context.Context, localPath, fileName string, size int64) (string, error) {
	token, err := getTenantToken(ctx)
	if err != nil {
		return "", err
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var werr error
		defer func() {
			if werr != nil {
				_ = pw.CloseWithError(werr)
			} else {
				_ = pw.Close()
			}
		}()
		fields := map[string]string{
			"file_name":   fileName,
			"parent_type": "bitable_file",
			"parent_node": bitableApp,
			"size":        strconv.FormatInt(size, 10),
		}
		for k, v := range fields {
			if werr = mw.WriteField(k, v); werr != nil {
				return
			}
		}
		var part io.Writer
		if part, werr = mw.CreateFormFile("file", fileName); werr != nil {
			return
		}
		var f *os.File
		if f, werr = os.Open(localPath); werr != nil {
			return
		}
		defer f.Close()
		if _, werr = io.Copy(part, f); werr != nil {
			return
		}
		werr = mw.Close()
	}()

	u := larkBase + "/drive/v1/medias/upload_all"
	req, _ := http.NewRequestWithContext(ctx, "POST", u, pr)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := apiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			FileToken string `json:"file_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Code != 0 || out.Data.FileToken == "" {
		return "", fmt.Errorf("upload_all code=%d msg=%s", out.Code, out.Msg)
	}
	return out.Data.FileToken, nil
}

// updateRecordFiles пишет file_token'ы в поле «Файлы» и отмечает «ФайлыЗагружены»=true.
// При markFunnelDone=true в той же атомарной PUT-записи поле «Воронка» переводится
// в «Документы скачал» (для карточек воронки «интересные»).
// При отсутствии полей — пробует создать их и повторить; если не вышло — обновляет без спорного поля.
func updateRecordFiles(ctx context.Context, recordID string, fileTokens []string, markFunnelDone bool) error {
	atts := make([]map[string]string, 0, len(fileTokens))
	for _, ft := range fileTokens {
		atts = append(atts, map[string]string{"file_token": ft})
	}
	fields := map[string]any{
		"Файлы":          atts,
		"ФайлыЗагружены": true,
	}
	if markFunnelDone {
		fields["Воронка"] = funnelDocsDownloaded
	}
	err := putRecord(ctx, recordID, fields)
	if err == nil {
		return nil
	}
	var le *larkError
	if !asLarkError(err, &le) || !isFieldNotFound(le.Code, le.Msg) {
		return err
	}
	log.Printf("[files] поле не найдено (%s), создаём и повторяем", le.Msg)
	_ = ensureBitableField(ctx, "Файлы", 17)
	_ = ensureBitableField(ctx, "ФайлыЗагружены", 7)
	_ = ensureBitableField(ctx, "Воронка", 1)
	if err = putRecord(ctx, recordID, fields); err == nil {
		return nil
	}
	// fallback: только вложения (и воронка, если запрошена), без чекбокса
	delete(fields, "ФайлыЗагружены")
	return putRecord(ctx, recordID, fields)
}

func putRecord(ctx context.Context, recordID string, fields map[string]any) error {
	u := fmt.Sprintf("%s/bitable/v1/apps/%s/tables/%s/records/%s", larkBase, bitableApp, bitableTable, recordID)
	body, err := larkJSON(ctx, "PUT", u, map[string]any{"fields": fields})
	if err != nil {
		return err
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(body, &out)
	if out.Code != 0 {
		return &larkError{Code: out.Code, Msg: out.Msg}
	}
	return nil
}

// findRecordsForFiles ищет карточки «На рассмотрении» без загруженных файлов.
// Основной фильтр: Статус="На рассмотрении" AND ФайлыЗагружены=false.
// Если поле ФайлыЗагружены отсутствует — fallback: Статус + пустое поле «Файлы».
func findRecordsForFiles(ctx context.Context) ([]bitableRecord, error) {
	fields := []string{"Номер", "Статус", "TenderplanID", "Файлы", "ФайлыЗагружены"}
	recs, err := bitableSearch(ctx, []map[string]any{
		{"field_name": "Статус", "operator": "is", "value": []string{"На рассмотрении"}},
		{"field_name": "ФайлыЗагружены", "operator": "is", "value": []string{"false"}},
	}, fields)
	if err == nil {
		return recs, nil
	}
	var le *larkError
	if !asLarkError(err, &le) || !isFieldNotFound(le.Code, le.Msg) {
		return nil, err
	}
	log.Printf("[files] поле ФайлыЗагружены отсутствует (%s), fallback на пустое поле «Файлы»", le.Msg)
	return bitableSearch(ctx, []map[string]any{
		{"field_name": "Статус", "operator": "is", "value": []string{"На рассмотрении"}},
		{"field_name": "Файлы", "operator": "isEmpty", "value": []string{}},
	}, []string{"Номер", "Статус", "TenderplanID", "Файлы"})
}

// dlFile — скачанный локально файл вложения (для загрузки в Lark и/или анализа ТЗ).
type dlFile struct {
	att  tpAttachment
	path string
	name string
	size int64
}

// findInterestingRecords ищет карточки воронки «интересные» без загруженных файлов.
// Основной фильтр: Воронка="интересные" AND ФайлыЗагружены=false.
// Если поле ФайлыЗагружены отсутствует — fallback: Воронка + пустое поле «Файлы».
func findInterestingRecords(ctx context.Context) ([]bitableRecord, error) {
	fields := []string{"Номер", "TenderplanID", "Файлы", "ФайлыЗагружены", "Воронка", "Анализ ТЗ"}
	recs, err := bitableSearch(ctx, []map[string]any{
		{"field_name": "Воронка", "operator": "is", "value": []string{"интересные"}},
		{"field_name": "ФайлыЗагружены", "operator": "is", "value": []string{"false"}},
	}, fields)
	if err == nil {
		return recs, nil
	}
	var le *larkError
	if !asLarkError(err, &le) || !isFieldNotFound(le.Code, le.Msg) {
		return nil, err
	}
	log.Printf("[воронки] поле ФайлыЗагружены отсутствует (%s), fallback на пустое поле «Файлы»", le.Msg)
	return bitableSearch(ctx, []map[string]any{
		{"field_name": "Воронка", "operator": "is", "value": []string{"интересные"}},
		{"field_name": "Файлы", "operator": "isEmpty", "value": []string{}},
	}, []string{"Номер", "TenderplanID", "Файлы", "Воронка", "Анализ ТЗ"})
}

// processFileDownloads — основной проход файлового пайплайна.
// Обрабатывает И карточки «На рассмотрении», И карточки воронки «интересные»
// (для интересных дноуглубительных после загрузки файлов запускается анализ ТЗ).
func processFileDownloads(ctx context.Context) fileRunResult {
	filesRunMu.Lock()
	defer filesRunMu.Unlock()

	res := fileRunResult{StartedAt: time.Now().Format(time.RFC3339)}
	defer func() {
		res.FinishedAt = time.Now().Format(time.RFC3339)
		reportFiles(res)
	}()

	ensureAnalysisFieldsOnce(ctx)

	recs, err := findRecordsForFiles(ctx)
	if err != nil {
		res.Errors = append(res.Errors, "search: "+err.Error())
		return res
	}
	log.Printf("[files] карточек «На рассмотрении» без файлов: %d", len(recs))

	// общая очередь: статус «На рассмотрении» + воронка «интересные» (дедуп по record_id)
	type queueItem struct {
		rec         bitableRecord
		interesting bool
	}
	queue := map[string]queueItem{}
	for _, rec := range recs {
		queue[rec.RecordID] = queueItem{rec: rec}
	}
	intRecs, ierr := findInterestingRecords(ctx)
	if ierr != nil {
		log.Printf("[воронки] поиск карточек «интересные»: %v", ierr)
		res.Errors = append(res.Errors, "[воронки] search interesting: "+ierr.Error())
	} else {
		log.Printf("[воронки] карточек «интересные» без файлов: %d", len(intRecs))
		for _, rec := range intRecs {
			queue[rec.RecordID] = queueItem{rec: rec, interesting: true}
		}
	}
	res.RecordsFound = len(queue)

	for _, it := range queue {
		// для интересных догружаем название тендера (признак «дноуглубительный»),
		// но только если анализ ТЗ ещё не сделан — чтобы не дёргать API зря
		name := ""
		if it.interesting && bitableText(it.rec.Fields["Анализ ТЗ"]) == "" {
			name = resolveTenderName(ctx, bitableText(it.rec.Fields["TenderplanID"]))
		}
		processRecordFiles(ctx, it.rec, name, it.interesting, &res)
	}
	return res
}

// processRecordFiles — скачивание и загрузка файлов одной карточки (общая логика
// для статуса «На рассмотрении» и воронки «интересные»).
// Семантика «все или ничего»: карточка обновляется только если ВСЕ файлы скачаны
// и загружены; при частичных ошибках карточку не трогаем — повтор в следующем проходе.
// Для интересных карточек (analyze=true) успешное обновление дополнительно переводит
// поле «Воронка» в «Документы скачал» (в т.ч. при отсутствии вложений).
// analyze=true + дноуглубительное название → после успешной загрузки файлов
// запускается первичный анализ ТЗ через Kimi.
func processRecordFiles(ctx context.Context, rec bitableRecord, tenderName string, analyze bool, res *fileRunResult) {
	number := bitableText(rec.Fields["Номер"])
	needAnalysis := analyze && tenderName != "" && dredgeRe.MatchString(tenderName) &&
		bitableText(rec.Fields["Анализ ТЗ"]) == ""

	// клиентская перестраховка: пропускаем, если файлы уже есть или чекбокс отмечен
	if done, _ := rec.Fields["ФайлыЗагружены"].(bool); done {
		// файлы уже загружены — остаётся только анализ ТЗ (если нужен)
		if needAnalysis {
			if err := runTZAnalysis(ctx, rec, nil); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: [анализ] %v", number, err))
			} else {
				res.TZAnalyses++
			}
		}
		return
	}
	if atts, ok := rec.Fields["Файлы"].([]any); ok && len(atts) > 0 {
		// страховка от «застрявших» карточек: файлы уже загружены (старой версией кода),
		// но воронка не переведена — догоняем переводом в «Документы скачал»
		if analyze && bitableText(rec.Fields["Воронка"]) != funnelDocsDownloaded {
			if err := putRecord(ctx, rec.RecordID, map[string]any{"Воронка": funnelDocsDownloaded}); err != nil {
				log.Printf("[воронки] %s: ошибка перевода воронки в «%s»: %v", number, funnelDocsDownloaded, err)
				res.Errors = append(res.Errors, fmt.Sprintf("%s: воронка→«%s»: %v", number, funnelDocsDownloaded, err))
			} else {
				log.Printf("[воронки] %s: файлы уже были, воронка → «%s»", number, funnelDocsDownloaded)
			}
		}
		return
	}
	tenderID := bitableText(rec.Fields["TenderplanID"])
	if tenderID == "" {
		res.Errors = append(res.Errors, number+": пустой TenderplanID")
		return
	}

	atts, err := fetchAttachments(ctx, tenderID)
	if err != nil {
		res.Errors = append(res.Errors, number+": attachments "+err.Error())
		return
	}
	if len(atts) == 0 {
		log.Printf("[files] %s (%s): файлов нет — отмечаем ФайлыЗагружены", number, tenderID)
		if err := updateRecordFiles(ctx, rec.RecordID, nil, analyze); err != nil {
			res.Errors = append(res.Errors, number+": mark-empty "+err.Error())
			return
		}
		res.RecordsDone++
		if analyze {
			log.Printf("[воронки] %s: документы скачаны (вложений нет), воронка → «%s»", number, funnelDocsDownloaded)
		}
		if needAnalysis {
			// вложений нет — зафиксируем это в поле «Анализ ТЗ», чтобы не возвращаться
			if err := runTZAnalysis(ctx, rec, nil); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: [анализ] %v", number, err))
			} else {
				res.TZAnalyses++
			}
		}
		return
	}

	// файлы-кандидаты на анализ ТЗ — их временные копии не удаляем сразу после загрузки
	candHrefs := map[string]bool{}
	if needAnalysis {
		for _, c := range pickTZCandidates(atts) {
			candHrefs[c.Href] = true
		}
	}

	var tokens []string
	var kept []dlFile
	failed := 0 // неудачные файлы этой записи (скачивание или загрузка в Lark)
	defer func() {
		for _, f := range kept {
			os.Remove(f.path) // временные файлы-кандидаты больше не нужны
		}
	}()
	for _, att := range atts {
		name := sanitizeFileName(att.RealName)
		if att.RealName == "" {
			name = sanitizeFileName(att.DisplayName)
		}
		path, size, err := downloadAttachment(ctx, att)
		if err != nil {
			failed++
			res.FilesFailed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %s download: %v", number, name, err))
			log.Printf("[files] %s: %s — ошибка скачивания: %v", number, name, err)
			continue
		}
		res.FilesDownloaded++
		token, err := uploadFileToDrive(ctx, path, name, size)
		if err != nil {
			os.Remove(path)
			failed++
			res.FilesFailed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %s upload: %v", number, name, err))
			log.Printf("[files] %s: %s (%d bytes) — ошибка загрузки в Lark: %v", number, name, size, err)
			continue
		}
		res.FilesUploaded++
		tokens = append(tokens, token)
		log.Printf("[files] %s: %s (%d bytes) — загружен, file_token=%s", number, name, size, token)
		if needAnalysis && candHrefs[att.Href] && size <= maxAnalysisFileSize {
			kept = append(kept, dlFile{att: att, path: path, name: name, size: size})
		} else {
			os.Remove(path) // временный файл больше не нужен
		}
		time.Sleep(200 * time.Millisecond)
	}

	// семантика «все или ничего»: при частичных ошибках карточку не обновляем —
	// частичный набор токенов не привязываем, повторим в следующем проходе
	if failed > 0 {
		log.Printf("[files] %s: неудачных файлов %d из %d — карточку не трогаем, будет повтор в следующем проходе", number, failed, len(atts))
		res.Errors = append(res.Errors, fmt.Sprintf("%s: частичная загрузка (%d из %d не удалось) — карточка не обновлена, повтор в следующем проходе", number, failed, len(atts)))
		return
	}
	if len(tokens) == 0 {
		return // все файлы упали — карточку не трогаем, повторим в следующем проходе
	}
	if err := updateRecordFiles(ctx, rec.RecordID, tokens, analyze && failed == 0); err != nil {
		res.Errors = append(res.Errors, number+": update record "+err.Error())
		return
	}
	res.RecordsDone++
	log.Printf("[files] %s: карточка обновлена, файлов: %d", number, len(tokens))
	if analyze {
		log.Printf("[воронки] %s: документы скачаны, воронка → «%s»", number, funnelDocsDownloaded)
	}

	// первичный анализ ТЗ (только дноуглубительные из воронки «интересные»)
	if needAnalysis {
		if err := runTZAnalysis(ctx, rec, kept); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: [анализ] %v", number, err))
		} else {
			res.TZAnalyses++
		}
	}
}

// scheduleFileDownloads — периодический опрос каждые filesPollInterval.
func scheduleFileDownloads(ctx context.Context) {
	go func() {
		t := time.NewTicker(filesPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				log.Printf("[files] плановый проход файлового пайплайна")
				res := processFileDownloads(ctx)
				log.Printf("[files] done: records=%d/%d downloaded=%d uploaded=%d failed=%d errors=%d",
					res.RecordsDone, res.RecordsFound, res.FilesDownloaded, res.FilesUploaded, res.FilesFailed, len(res.Errors))
			}
		}
	}()
}

// ===== TENDERPLAN MARKS (воронка «интересные») =====
// Опрос подписки Tenderplan v1 (/api/tenders/getlist): тендеры с меткой, чьё имя
// матчится (?i)интерес, попадают в воронку «интересные» + принудительный файловый пайплайн.

// tpSubTender — модель тендера подписки из v1 /api/tenders/getlist (+ поле marks).
type tpSubTender struct {
	tpShortTender
	Marks []json.RawMessage `json:"marks"` // массив ObjectId меток (строки или объекты)
}

// markIDs извлекает ObjectId меток (терпимо к формату: строка / {"_id":...} / {"$oid":...}).
func (s *tpSubTender) markIDs() []string {
	var ids []string
	for _, raw := range s.Marks {
		var id string
		if err := json.Unmarshal(raw, &id); err == nil && id != "" {
			ids = append(ids, id)
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err == nil {
			for _, k := range []string{"_id", "id", "$oid"} {
				if v, ok := obj[k].(string); ok && v != "" {
					ids = append(ids, v)
					break
				}
			}
		}
	}
	return ids
}

// tenderFromSub конвертирует модель подписки во внутреннюю модель Tender.
func tenderFromSub(s *tpSubTender) Tender {
	t := Tender{
		ID:       s.ID,
		Number:   s.Number,
		Name:     s.OrderName,
		Region:   rawToString(s.Region),
		Title:    s.OrderName,
		Deadline: msToRFC3339(s.SubmissionCloseDateTime),
		URL:      s.Href,
	}
	if s.MaxPrice != nil {
		t.Amount = *s.MaxPrice
	}
	if len(s.Customers) > 0 {
		t.Customer = s.Customers[0].Name
		if t.Region == "" {
			t.Region = strconv.Itoa(s.Customers[0].Region)
		}
	}
	return t
}

// fetchSubscriptionPage — одна страница v1 getlist (50/стр., тот же Bearer-ключ).
func fetchSubscriptionPage(ctx context.Context, page int) ([]tpSubTender, error) {
	var raw json.RawMessage
	u := fmt.Sprintf("%s/tenders/getlist?page=%d", tenderplanAPI, page)
	if err := tpGet(ctx, u, &raw); err != nil {
		return nil, err
	}
	// форматы ответа: {"tenders":[...]} / {"data":[...]} / {"items":[...]} / сразу массив
	var wrap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrap); err == nil {
		for _, key := range []string{"tenders", "data", "items"} {
			if arr, ok := wrap[key]; ok {
				var out []tpSubTender
				if err := json.Unmarshal(arr, &out); err == nil {
					return out, nil
				}
			}
		}
	}
	var out []tpSubTender
	if err := json.Unmarshal(raw, &out); err == nil {
		return out, nil
	}
	return nil, fmt.Errorf("getlist v1: неизвестный формат ответа: %.200s", string(raw))
}

// markNameCache — кэш имён меток Tenderplan (map+mutex).
var markNameCache = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// resolveMarkName резолвит имя метки по ObjectId: GET /api/marks/get?id=<ObjectId> (с кэшем).
func resolveMarkName(ctx context.Context, id string) (string, error) {
	markNameCache.Lock()
	if v, ok := markNameCache.m[id]; ok {
		markNameCache.Unlock()
		return v, nil
	}
	markNameCache.Unlock()
	var raw json.RawMessage
	if err := tpGet(ctx, tenderplanAPI+"/marks/get?id="+url.QueryEscape(id), &raw); err != nil {
		return "", err
	}
	var out struct {
		Name string `json:"name"`
		Mark *struct {
			Name string `json:"name"`
		} `json:"mark"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	name := out.Name
	if name == "" && out.Mark != nil {
		name = out.Mark.Name
	}
	markNameCache.Lock()
	markNameCache.m[id] = name
	markNameCache.Unlock()
	return name, nil
}

// hasInterestingMark проверяет, есть ли у тендера метка с именем (?i)интерес.
func hasInterestingMark(ctx context.Context, s *tpSubTender) bool {
	for _, mid := range s.markIDs() {
		name, err := resolveMarkName(ctx, mid)
		if err != nil {
			log.Printf("[метки] marks/get %s: %v", mid, err)
			continue
		}
		if interestingMarkRe.MatchString(name) {
			return true
		}
	}
	return false
}

// recordSearchFields — поля, нужные файловому пайплайну/воронкам при поиске записи.
var recordSearchFields = []string{"Номер", "TenderplanID", "Файлы", "ФайлыЗагружены", "Воронка", "Анализ ТЗ"}

// findRecordByTenderplanID — поиск записи по полю TenderplanID (nil, nil — не найдено).
func findRecordByTenderplanID(ctx context.Context, id string) (*bitableRecord, error) {
	cond := []map[string]any{
		{"field_name": "TenderplanID", "operator": "is", "value": []string{id}},
	}
	recs, err := bitableSearch(ctx, cond, recordSearchFields)
	if err != nil {
		// возможно, каких-то опциональных полей ещё нет — повторяем с базовым набором
		recs, err = bitableSearch(ctx, cond, []string{"Номер", "TenderplanID", "Файлы", "ФайлыЗагружены"})
		if err != nil {
			return nil, err
		}
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return &recs[0], nil
}

// findRecordByNumberFull — поиск записи по полю «Номер» (fallback, когда нет поля TenderplanID).
func findRecordByNumberFull(ctx context.Context, number string) (*bitableRecord, error) {
	recs, err := bitableSearch(ctx, []map[string]any{
		{"field_name": "Номер", "operator": "is", "value": []string{number}},
	}, []string{"Номер", "TenderplanID", "Файлы", "ФайлыЗагружены"})
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return &recs[0], nil
}

// interestingItem — строка отчёта по интересному тендеру (название + статус обработки).
type interestingItem struct {
	Name   string `json:"name"`
	Number string `json:"number,omitempty"`
	Status string `json:"status"`
}

// marksResult — итоги одного прохода опроса меток.
type marksResult struct {
	Checked     int      `json:"checked"`
	Interesting int      `json:"interesting"`
	Created     int      `json:"created"`
	Updated     int      `json:"updated"`
	Errors      []string `json:"errors,omitempty"`
}

var marksRunMu sync.Mutex // защита от параллельных запусков опроса меток

// processMarkedTenders — опрос меток Tenderplan: тендеры с меткой «интерес...»
// заводятся/обновляются в воронку «интересные» + принудительный запуск файлов и анализа ТЗ.
// Ошибки API собираются в res.Errors и логируются, цикл не роняют.
func processMarkedTenders(ctx context.Context) marksResult {
	if !marksRunMu.TryLock() {
		log.Printf("[метки] предыдущий опрос меток ещё идёт — пропуск")
		return marksResult{Errors: []string{"previous marks poll still running"}}
	}
	defer marksRunMu.Unlock()

	res := marksResult{}
	var items []interestingItem
	defer func() { reportMarks(res.Interesting, items, res.Errors) }()

	ensureAnalysisFieldsOnce(ctx)

	// 1. собираем интересные тендеры подписки (пагинация v1 getlist)
	seen := map[string]bool{}
	var interesting []tpSubTender
	for page := 0; page < marksMaxPages; page++ {
		subs, err := fetchSubscriptionPage(ctx, page)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("getlist page=%d: %v", page, err))
			break
		}
		if len(subs) == 0 {
			break
		}
		for i := range subs {
			s := &subs[i]
			res.Checked++
			if s.ID == "" || seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			if len(s.Marks) == 0 || !hasInterestingMark(ctx, s) {
				continue
			}
			interesting = append(interesting, *s)
		}
		if len(subs) < 50 { // последняя страница
			break
		}
	}
	res.Interesting = len(interesting)
	log.Printf("[метки] проверено тендеров подписки: %d, интересных: %d", res.Checked, res.Interesting)

	// 2. обрабатываем каждый интересный тендер
	for i := range interesting {
		s := &interesting[i]
		item := interestingItem{Name: s.OrderName, Number: s.Number}
		rec, err := findRecordByTenderplanID(ctx, s.ID)
		if err != nil {
			var le *larkError
			if asLarkError(err, &le) && isFieldNotFound(le.Code, le.Msg) {
				log.Printf("[метки] поле TenderplanID отсутствует (%s), fallback на поиск по номеру", le.Msg)
				rec, err = findRecordByNumberFull(ctx, s.Number)
			}
		}
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: bitable search: %v", s.Number, err))
			item.Status = "ошибка поиска в Bitable: " + err.Error()
			items = append(items, item)
			continue
		}

		if rec == nil {
			// записи нет — догружаем полную карточку и создаём (Kimi-резюме опционально)
			t := tenderFromSub(s)
			if d, derr := fetchTenderDetails(ctx, s.ID); derr == nil {
				if t.URL == "" {
					t.URL = d.Href
				}
				if d.OrderName != "" {
					t.Name = d.OrderName
					t.Title = d.OrderName
				}
				t.Description = d.Description
				if t.Description == "" {
					t.Description = d.NoticeDescription
				}
			} else {
				log.Printf("[метки] полная карточка %s: %v", s.ID, derr)
			}
			summary, kerr := kimiSummarize(ctx, t)
			if kerr != nil {
				log.Printf("[метки] kimi %s: %v", s.Number, kerr)
				res.Errors = append(res.Errors, s.Number+": kimi "+kerr.Error())
				summary = "⚠️ Резюме недоступно: ошибка Kimi API (см. логи). Данные тендера — в полях карточки."
			}
			r, err := addToBitable(ctx, t, classifyCluster(t), summary, "интересные")
			if err != nil {
				res.Errors = append(res.Errors, s.Number+": bitable "+err.Error())
				item.Status = "ошибка создания записи: " + err.Error()
				items = append(items, item)
				continue
			}
			if r.Status == "duplicate" {
				item.Status = "дубль по номеру — запись уже существовала"
				items = append(items, item)
				continue
			}
			res.Created++
			recID := recordIDFromCreate(r.Record)
			rec = &bitableRecord{RecordID: recID, Fields: map[string]any{
				"Номер": s.Number, "TenderplanID": s.ID, "Воронка": "интересные",
			}}
			item.Status = "создана запись, воронка «интересные»"
			log.Printf("[воронки] %s: создана запись (воронка «интересные»), record=%s", s.Number, recID)
		} else {
			// запись уже есть — обновляем воронку на «интересные», не дублируя запись
			if bitableText(rec.Fields["Воронка"]) != "интересные" {
				if err := putRecord(ctx, rec.RecordID, map[string]any{"Воронка": "интересные"}); err != nil {
					res.Errors = append(res.Errors, s.Number+": update voronka "+err.Error())
					item.Status = "запись есть, ошибка обновления воронки: " + err.Error()
					items = append(items, item)
					continue
				}
				res.Updated++
				item.Status = "запись уже была — воронка обновлена на «интересные»"
				log.Printf("[воронки] %s: воронка обновлена на «интересные», record=%s", s.Number, rec.RecordID)
			} else {
				item.Status = "уже в воронке «интересные»"
			}
		}

		// принудительное скачивание файлов (+ анализ ТЗ для дноуглубительных)
		if rec.RecordID == "" {
			items = append(items, item)
			continue
		}
		if done, _ := rec.Fields["ФайлыЗагружены"].(bool); done {
			// файлы уже загружены — при необходимости только анализ ТЗ
			if dredgeRe.MatchString(s.OrderName) && bitableText(rec.Fields["Анализ ТЗ"]) == "" {
				if err := runTZAnalysis(ctx, *rec, nil); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: [анализ] %v", s.Number, err))
					item.Status += "; анализ ТЗ: " + err.Error()
				} else {
					item.Status += "; анализ ТЗ выполнен"
				}
			}
			items = append(items, item)
			continue
		}
		if !filesRunMu.TryLock() {
			// плановый файловый пайплайн идёт прямо сейчас — он подхватит карточку сам
			log.Printf("[метки] %s: файловый пайплайн занят — файлы подхватит плановый проход", s.Number)
			item.Status += "; файлы — в плановом проходе пайплайна"
			items = append(items, item)
			continue
		}
		fres := fileRunResult{StartedAt: time.Now().Format(time.RFC3339)}
		processRecordFiles(ctx, *rec, s.OrderName, true, &fres)
		fres.FinishedAt = time.Now().Format(time.RFC3339)
		filesRunMu.Unlock()
		res.Errors = append(res.Errors, fres.Errors...)
		switch {
		case fres.RecordsDone > 0 && fres.TZAnalyses > 0:
			item.Status += "; файлы загружены, анализ ТЗ выполнен"
		case fres.RecordsDone > 0:
			item.Status += "; файлы загружены"
		default:
			item.Status += "; файлы не обработаны (см. ошибки)"
		}
		items = append(items, item)
	}
	return res
}

// scheduleMarksPoll — периодический опрос меток Tenderplan каждые marksPollInterval.
func scheduleMarksPoll(ctx context.Context) {
	go func() {
		t := time.NewTicker(marksPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				log.Printf("[метки] плановый опрос меток Tenderplan")
				res := processMarkedTenders(ctx)
				log.Printf("[метки] done: checked=%d interesting=%d created=%d updated=%d errors=%d",
					res.Checked, res.Interesting, res.Created, res.Updated, len(res.Errors))
			}
		}
	}()
}

// ===== АНАЛИЗ ТЗ (KIMI FILES API) =====
// Первичный анализ ТЗ дноуглубительных тендеров из воронки «интересные».

// ensureAnalysisFieldsOnce создаёт новые поля Bitable (Воронка + поля анализа ТЗ) один раз за процесс.
var analysisFieldsOnce sync.Once

func ensureAnalysisFieldsOnce(ctx context.Context) {
	analysisFieldsOnce.Do(func() {
		for _, fn := range []string{"Воронка", "Анализ ТЗ", "Объём грунта", "Отвал: расстояние", "Техника", "Стоимость 1 м³"} {
			_ = ensureBitableField(ctx, fn, 1) // ошибки (в т.ч. «уже существует») логируются внутри
		}
	})
}

// tenderNameCache — кэш названий тендеров (для проверки «дноуглубительный» в файловом пайплайне).
var tenderNameCache = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// resolveTenderName возвращает название тендера по TenderplanID (с кэшем; при ошибке — пустую строку).
func resolveTenderName(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	tenderNameCache.Lock()
	if v, ok := tenderNameCache.m[id]; ok {
		tenderNameCache.Unlock()
		return v
	}
	tenderNameCache.Unlock()
	d, err := fetchTenderDetails(ctx, id)
	if err != nil {
		log.Printf("[анализ] название тендера %s: %v", id, err)
		return "" // ошибки не кэшируем — повторим в следующем проходе
	}
	tenderNameCache.Lock()
	tenderNameCache.m[id] = d.OrderName
	tenderNameCache.Unlock()
	return d.OrderName
}

// pickTZCandidates выбирает файлы для анализа ТЗ: сначала имена с «тз/техническ/задание»,
// иначе — до 3 первых документов. Файлы >50 МБ пропускаются (если размер известен).
func pickTZCandidates(atts []tpAttachment) []tpAttachment {
	var tz, rest []tpAttachment
	for _, a := range atts {
		if a.Size > maxAnalysisFileSize {
			continue
		}
		name := a.RealName
		if name == "" {
			name = a.DisplayName
		}
		if tzFileRe.MatchString(name) {
			tz = append(tz, a)
		} else {
			rest = append(rest, a)
		}
	}
	src := tz
	if len(src) == 0 {
		src = rest
	}
	if len(src) > 3 {
		src = src[:3]
	}
	return src
}

// analysisAttempts — защита от повторных прогонов Kimi по одной записи (ошибки API, цикл каждые 5 мин).
var analysisAttempts = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

// markAnalysisAttempt фиксирует попытку анализа; false — если попытка была < analysisRetryDelay назад.
func markAnalysisAttempt(recordID string) bool {
	analysisAttempts.Lock()
	defer analysisAttempts.Unlock()
	if ts, ok := analysisAttempts.m[recordID]; ok && time.Since(ts) < analysisRetryDelay {
		return false
	}
	analysisAttempts.m[recordID] = time.Now()
	return true
}

// runTZAnalysis — первичный анализ ТЗ дноуглубительного тендера через Kimi Files API.
// local — уже скачанные файлы-кандидаты (nil → выбрать и скачать самостоятельно по TenderplanID).
// Результат пишется в поля «Анализ ТЗ», «Объём грунта», «Отвал: расстояние», «Техника», «Стоимость 1 м³».
func runTZAnalysis(ctx context.Context, rec bitableRecord, local []dlFile) error {
	number := bitableText(rec.Fields["Номер"])
	if !markAnalysisAttempt(rec.RecordID) {
		return fmt.Errorf("повторная попытка раньше чем через %s — пропуск", analysisRetryDelay)
	}
	if local == nil {
		tenderID := bitableText(rec.Fields["TenderplanID"])
		if tenderID == "" {
			return fmt.Errorf("пустой TenderplanID — нечего анализировать")
		}
		atts, err := fetchAttachments(ctx, tenderID)
		if err != nil {
			return fmt.Errorf("attachments: %w", err)
		}
		cands := pickTZCandidates(atts)
		if len(cands) == 0 {
			log.Printf("[анализ] %s: нет файлов ТЗ для анализа — фиксируем пометку", number)
			return writeAnalysisNote(ctx, rec.RecordID,
				"Файлы ТЗ во вложениях тендера не найдены (или все >50 МБ) — первичный анализ невозможен.")
		}
		var downloaded []dlFile
		defer func() {
			for _, f := range downloaded {
				os.Remove(f.path)
			}
		}()
		for _, att := range cands {
			name := sanitizeFileName(att.RealName)
			if att.RealName == "" {
				name = sanitizeFileName(att.DisplayName)
			}
			path, size, err := downloadAttachment(ctx, att)
			if err != nil {
				log.Printf("[анализ] %s: %s — ошибка скачивания: %v", number, name, err)
				continue
			}
			if size > maxAnalysisFileSize {
				log.Printf("[анализ] %s: %s (%d bytes) > 50 МБ — пропуск", number, name, size)
				os.Remove(path)
				continue
			}
			downloaded = append(downloaded, dlFile{att: att, path: path, name: name, size: size})
		}
		local = downloaded
	}
	if len(local) == 0 {
		return fmt.Errorf("файлы ТЗ не скачаны — анализ отложен")
	}

	// извлекаем текст первого файла, с которым справилась Kimi
	var tzText, usedFile string
	for _, f := range local {
		fileID, err := kimiUploadFile(ctx, f.path, f.name)
		if err != nil {
			log.Printf("[анализ] %s: загрузка %s в Kimi: %v", number, f.name, err)
			continue
		}
		content, err := kimiFileContent(ctx, fileID)
		if err != nil {
			log.Printf("[анализ] %s: контент %s из Kimi: %v", number, f.name, err)
			continue
		}
		if strings.TrimSpace(content) == "" {
			log.Printf("[анализ] %s: %s — Kimi вернула пустой текст, пробуем следующий файл", number, f.name)
			continue
		}
		tzText, usedFile = content, f.name
		break
	}
	if tzText == "" {
		return fmt.Errorf("Kimi не смогла извлечь текст ни из одного файла ТЗ")
	}
	log.Printf("[анализ] %s: текст ТЗ извлечён из %q (%d знаков), запускаем Kimi-анализ", number, usedFile, len([]rune(tzText)))

	vals, full, err := kimiAnalyzeTZ(ctx, tzText)
	if err != nil {
		return err
	}
	if err := writeAnalysisFields(ctx, rec.RecordID, vals, full); err != nil {
		return err
	}
	log.Printf("[анализ] %s: анализ ТЗ записан в карточку (файл %q)", number, usedFile)
	return nil
}

// kimiUploadFile загружает файл в Kimi Files API (multipart, purpose=file-extract) и возвращает file id.
func kimiUploadFile(ctx context.Context, localPath, fileName string) (string, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var werr error
		defer func() {
			if werr != nil {
				_ = pw.CloseWithError(werr)
			} else {
				_ = pw.Close()
			}
		}()
		if werr = mw.WriteField("purpose", "file-extract"); werr != nil {
			return
		}
		var part io.Writer
		if part, werr = mw.CreateFormFile("file", fileName); werr != nil {
			return
		}
		var f *os.File
		if f, werr = os.Open(localPath); werr != nil {
			return
		}
		defer f.Close()
		if _, werr = io.Copy(part, f); werr != nil {
			return
		}
		werr = mw.Close()
	}()

	req, _ := http.NewRequestWithContext(ctx, "POST", kimiFilesAPI, pr)
	req.Header.Set("Authorization", "Bearer "+kimiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := kimiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		ID    string `json:"id"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("kimi files: bad json: %.200s", string(body))
	}
	if out.Error != nil {
		return "", fmt.Errorf("kimi files: %s", out.Error.Message)
	}
	if resp.StatusCode != http.StatusOK || out.ID == "" {
		return "", fmt.Errorf("kimi files HTTP %d: %.200s", resp.StatusCode, string(body))
	}
	return out.ID, nil
}

// kimiFileContent возвращает извлечённый текст файла (GET /v1/files/{id}/content).
func kimiFileContent(ctx context.Context, fileID string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", kimiFilesAPI+"/"+fileID+"/content", nil)
	req.Header.Set("Authorization", "Bearer "+kimiKey)
	resp, err := kimiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("kimi file content HTTP %d: %.200s", resp.StatusCode, string(body))
	}
	// обычно ответ — JSON {"content": "...", ...}; на всякий случай принимаем и сырой текст
	var out struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &out); err == nil && out.Content != "" {
		return out.Content, nil
	}
	return string(body), nil
}

// tzAnalysisValues — структурированный результат анализа ТЗ (просим у Kimi строгий JSON).
type tzAnalysisValues struct {
	Obem    string `json:"obem"`
	Otval   string `json:"otval"`
	Tehnika string `json:"tehnika"`
	CenaM3  string `json:"cena_m3"`
	Komment string `json:"komment"`
}

// kimiAnalyzeTZ прогоняет текст ТЗ через Kimi: system = содержимое ТЗ, user = инструкция
// извлечь 4 пункта и вернуть JSON. Возвращает извлечённые значения + полный текст ответа.
func kimiAnalyzeTZ(ctx context.Context, tzText string) (tzAnalysisValues, string, error) {
	var vals tzAnalysisValues
	const maxSystemRunes = 120000 // защита от переполнения контекста на очень больших ТЗ
	if r := []rune(tzText); len(r) > maxSystemRunes {
		tzText = string(r[:maxSystemRunes])
		log.Printf("[анализ] текст ТЗ обрезан до %d знаков (было %d)", maxSystemRunes, len(r))
	}
	prompt := `Это техническое задание (ТЗ) дноуглубительных работ. Извлеки из него ровно 4 пункта:
1) obem — объём грунта выемки/отсыпки (м³);
2) otval — расстояние от места производства работ до отвала;
3) tehnika — необходимая техника;
4) cena_m3 — стоимость 1 м³ вынутого и перемещённого грунта; если цены в ТЗ нет — оцени как НМЦ/объём и ЯВНО пометь «оценка»;
5) komment — краткий комментарий (1-2 предложения).
Если каких-то данных нет — напиши «не указано в ТЗ».
Ответ краткий, по-русски, структурированный. Ответь СТРОГО одним JSON-объектом без пояснений и markdown:
{"obem":"...","otval":"...","tehnika":"...","cena_m3":"...","komment":"..."}`
	body, _ := json.Marshal(map[string]any{
		"model": kimiModel,
		"messages": []map[string]string{
			{"role": "system", "content": tzText},
			{"role": "user", "content": prompt},
		},
		// reasoning-модель съедает токены на «размышления» — нужен запас (см. kimiSummarize);
		// temperature НЕ передаём: kimi-k2.6 принимает только temperature=1.
		"max_tokens": 4096,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", kimiAPI, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+kimiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := kimiClient.Do(req)
	if err != nil {
		return vals, "", err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return vals, "", err
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" {
		return vals, "", fmt.Errorf("kimi: empty response")
	}
	full := strings.TrimSpace(out.Choices[0].Message.Content)
	// парсим JSON из ответа (модель может обернуть его в ```json ... ```);
	// при ошибке парсинга полный текст всё равно уйдёт в поле «Анализ ТЗ».
	if i, j := strings.Index(full, "{"), strings.LastIndex(full, "}"); i >= 0 && j > i {
		if err := json.Unmarshal([]byte(full[i:j+1]), &vals); err != nil {
			log.Printf("[анализ] не удалось распарсить JSON из ответа Kimi: %v — пишем весь текст в «Анализ ТЗ»", err)
		}
	}
	return vals, full, nil
}

// writeAnalysisFields пишет результат анализа в карточку с самолечением «поле не существует».
func writeAnalysisFields(ctx context.Context, recordID string, vals tzAnalysisValues, full string) error {
	fields := map[string]any{"Анализ ТЗ": full}
	if vals.Obem != "" {
		fields["Объём грунта"] = vals.Obem
	}
	if vals.Otval != "" {
		fields["Отвал: расстояние"] = vals.Otval
	}
	if vals.Tehnika != "" {
		fields["Техника"] = vals.Tehnika
	}
	if vals.CenaM3 != "" {
		fields["Стоимость 1 м³"] = vals.CenaM3
	}
	err := putRecord(ctx, recordID, fields)
	if err == nil {
		return nil
	}
	var le *larkError
	if !asLarkError(err, &le) || !isFieldNotFound(le.Code, le.Msg) {
		return err
	}
	log.Printf("[анализ] поле не найдено (%s), создаём и повторяем", le.Msg)
	for _, fn := range []string{"Анализ ТЗ", "Объём грунта", "Отвал: расстояние", "Техника", "Стоимость 1 м³"} {
		_ = ensureBitableField(ctx, fn, 1)
	}
	if err = putRecord(ctx, recordID, fields); err == nil {
		return nil
	}
	// fallback: хотя бы полный текст анализа
	return putRecord(ctx, recordID, map[string]any{"Анализ ТЗ": full})
}

// writeAnalysisNote фиксирует служебную пометку в поле «Анализ ТЗ» (чтобы не анализировать повторно).
func writeAnalysisNote(ctx context.Context, recordID, note string) error {
	if err := putRecord(ctx, recordID, map[string]any{"Анализ ТЗ": note}); err != nil {
		var le *larkError
		if asLarkError(err, &le) && isFieldNotFound(le.Code, le.Msg) {
			_ = ensureBitableField(ctx, "Анализ ТЗ", 1)
			return putRecord(ctx, recordID, map[string]any{"Анализ ТЗ": note})
		}
		return err
	}
	return nil
}

// ===== REPORT (GET /report) =====
// In-memory состояние последних прогонов: ежедневный сбор, метки, файлы, анализ ТЗ.

type reportData struct {
	Time             string            `json:"time"`
	NewAdded         int               `json:"new_added"`         // новых добавлено (последний ежедневный сбор)
	InterestingFound int               `json:"interesting_found"` // интересных найдено (последний опрос меток)
	FilesDownloaded  int               `json:"files_downloaded"`  // файлов скачано (последний проход пайплайна)
	TZAnalyses       int               `json:"tz_analyses"`       // анализов ТЗ сделано (последний проход пайплайна)
	Errors           []string          `json:"errors"`            // ошибки всех подсистем одним списком
	Interesting      []interestingItem `json:"interesting,omitempty"`
}

var report = struct {
	sync.Mutex
	data                            reportData
	dailyErrs, marksErrs, filesErrs []string
}{data: reportData{Errors: []string{}}}

// reportDaily фиксирует итоги ежедневного сбора (новые → воронка «новые от ...»).
func reportDaily(res runResults) {
	report.Lock()
	defer report.Unlock()
	report.data.Time = time.Now().Format(time.RFC3339)
	report.data.NewAdded = res.Added
	report.dailyErrs = res.Errors
}

// reportMarks фиксирует итоги опроса меток Tenderplan (воронка «интересные»).
func reportMarks(found int, items []interestingItem, errs []string) {
	report.Lock()
	defer report.Unlock()
	report.data.Time = time.Now().Format(time.RFC3339)
	report.data.InterestingFound = found
	report.data.Interesting = items
	report.marksErrs = errs
}

// reportFiles фиксирует итоги последнего прохода файлового пайплайна.
func reportFiles(res fileRunResult) {
	report.Lock()
	defer report.Unlock()
	report.data.Time = time.Now().Format(time.RFC3339)
	report.data.FilesDownloaded = res.FilesDownloaded
	report.data.TZAnalyses = res.TZAnalyses
	report.filesErrs = res.Errors
}

// reportSnapshot — снимок состояния для GET /report (ошибки всех подсистем — одним списком).
func reportSnapshot() reportData {
	report.Lock()
	defer report.Unlock()
	out := report.data
	out.Errors = append([]string{}, report.dailyErrs...)
	out.Errors = append(out.Errors, report.marksErrs...)
	out.Errors = append(out.Errors, report.filesErrs...)
	return out
}

// ===== HMAC VERIFY =====

func verifyHMAC(body []byte, signature, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expected))
}

// ===== HTTP HANDLERS =====

type mailTenderIn struct {
	Tenders []Tender `json:"tenders"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "service": "tender-monitor"})
}

var monitorRunMu sync.Mutex // защита от параллельных запусков мониторинга

func (h *server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Полный цикл долгий (Kimi по каждому тендеру): HTTP-запрос отвалится раньше.
	// Запускаем в фоне с собственным контекстом — отключение клиента не убивает работу.
	if !monitorRunMu.TryLock() {
		writeJSON(w, 409, map[string]string{"status": "already_running"})
		return
	}
	go func() {
		defer monitorRunMu.Unlock()
		log.Printf("[run] старт полного цикла мониторинга")
		res := processTenders(context.Background())
		reportDaily(res)
		log.Printf("[run] done: processed=%d added=%d duplicates=%d errors=%d %v",
			res.Processed, res.Added, res.Duplicates, len(res.Errors), res.Errors)
	}()
	writeJSON(w, 202, map[string]string{"status": "started", "hint": "смотрите логи контейнера: [run] done: ..."})
}

// handleReport — GET /report: состояние последних прогонов
// (ежедневный сбор, метки/воронки, файловый пайплайн, анализ ТЗ).
func (h *server) handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, 200, reportSnapshot())
}

// handleFilesRun — ручной запуск файлового пайплайна: POST /files/run.
func (h *server) handleFilesRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	res := processFileDownloads(r.Context())
	writeJSON(w, 200, res)
}

func (h *server) handleMailTenders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "read body"})
		return
	}
	sig := r.Header.Get("X-Mail-Signature")
	if mailSecret != "" {
		if !verifyHMAC(body, sig, mailSecret) {
			writeJSON(w, 401, map[string]string{"error": "bad signature"})
			return
		}
	}
	var in mailTenderIn
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	res := processMailTenders(r.Context(), in.Tenders)
	writeJSON(w, 200, res)
}

func (h *server) handleListTenders(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	token, err := getTenantToken(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	u := fmt.Sprintf("%s/bitable/v1/apps/%s/tables/%s/records/search", larkBase, bitableApp, bitableTable)
	bodyMap := map[string]any{}
	if status != "" {
		bodyMap["filter"] = map[string]any{"conjunction": "and", "conditions": []any{
			map[string]any{"field_name": "Статус", "operator": "is", "value": []string{status}},
		}}
	}
	body, _ := json.Marshal(bodyMap)
	req, _ := http.NewRequestWithContext(r.Context(), "POST", u, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := apiClient.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rb)
}

// ===== SCHEDULER =====

func scheduleDaily(ctx context.Context) {
	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), dailyRunHour, dailyRunMinute, 0, 0, now.Location())
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			wait := time.Until(next)
			log.Printf("[scheduler] next run at %s (in %s)", next.Format(time.RFC3339), wait.Round(time.Second))
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				log.Printf("[scheduler] running daily job")
				res := processTenders(ctx)
				reportDaily(res)
				log.Printf("[scheduler] done: processed=%d added=%d duplicates=%d errors=%d",
					res.Processed, res.Added, res.Duplicates, len(res.Errors))
			}
		}
	}()
}

type server struct{}

// init устанавливает глобальный DNS-резолвер ДО запуска любого кода.
// DNS-обход: встроенный DNS Docker swarm мёртв (iptables выключен на dockerd),
// поэтому резолвим напрямую через внешние DNS по UDP.
func init() {
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			// 77.88.8.8 — DNS Яндекса (быстрый в РФ), 8.8.8.8 — запасной
			for _, srv := range []string{"77.88.8.8:53", "8.8.8.8:53"} {
				conn, err := d.DialContext(ctx, "udp", srv)
				if err == nil {
					return conn, nil
				}
			}
			return d.DialContext(ctx, "udp", "77.88.8.8:53")
		},
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if tenderplanKey == "" {
		log.Println("[warn] TENDERPLAN_KEY is empty")
	}
	if kimiKey == "" {
		log.Println("[warn] KIMI_KEY is empty")
	}
	if larkAppSecret == "" {
		log.Println("[warn] LARK_APP_SECRET is empty")
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduleDaily(rootCtx)
	scheduleFileDownloads(rootCtx)
	scheduleMarksPoll(rootCtx)

	mux := http.NewServeMux()
	h := &server{}
	mux.HandleFunc("/", h.handleRoot)
	mux.HandleFunc("/run", h.handleRun)
	mux.HandleFunc("/files/run", h.handleFilesRun)
	mux.HandleFunc("/webhook/mail-tenders", h.handleMailTenders)
	mux.HandleFunc("/tenders", h.handleListTenders)
	mux.HandleFunc("/report", h.handleReport)
	mux.Handle("/reports/", http.StripPrefix("/reports/", http.FileServer(http.Dir("/app/reports"))))

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("[tender-monitor] listening on %s", listenAddr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
