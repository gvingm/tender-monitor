// Package main — Tender Monitor: мониторинг тендеров Tenderplan + Lark Bitable + Kimi-резюме
// + скачивание файлов тендера при переходе карточки в статус «На рассмотрении».
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
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ===== CONFIG =====

const (
	tenderplanAPI       = "https://tenderplan.ru/api"
	kimiAPI             = "https://api.moonshot.ai/v1/chat/completions"
	kimiModel           = "kimi-k2.6"
	larkBase            = "https://open.larksuite.com/open-apis"
	listenAddr          = "0.0.0.0:8787" // IPv4 явно: WSL localhostForwarding не пробрасывает tcp6-only сокеты
	dailyRunHour        = 8
	dailyRunMinute      = 0
	filesPollInterval   = 5 * time.Minute // периодический опрос карточек «На рассмотрении»
	fileDownloadTimeout = 60 * time.Second // таймаут на скачивание одного файла
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
	Tender  *struct {
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

// fetchTenderHref догружает полную модель тендера ради ссылки на площадку-источник.
func fetchTenderHref(ctx context.Context, id string) (string, error) {
	var out struct {
		Href string `json:"href"`
		// некоторые версии API заворачивают модель
		Tender *struct {
			Href string `json:"href"`
		} `json:"tender"`
	}
	if err := tpGet(ctx, tenderplanAPI+"/tenders/get?id="+url.QueryEscape(id), &out); err != nil {
		return "", err
	}
	if out.Href != "" {
		return out.Href, nil
	}
	if out.Tender != nil {
		return out.Tender.Href, nil
	}
	return "", nil
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
// TenderplanID пишется для последующего скачивания файлов; при ошибке «поле не существует»
// поле создаётся, а если и это не удалось — запись создаётся без него.
func addToBitable(ctx context.Context, t Tender, cluster, summary string) (bitableResult, error) {
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
			code, rec, err = createBitableRecord(ctx, fields)
			if err != nil {
				// не падаем: повторяем без спорного поля
				if ok := asLarkError(err, &le); ok && isFieldNotFound(le.Code, le.Msg) {
					delete(fields, "TenderplanID")
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
			r, err := addToBitable(ctx, t, name, summary)
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
		r, err := addToBitable(ctx, t, cluster, summary)
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
// При отсутствии полей — пробует создать их и повторить; если не вышло — обновляет без спорного поля.
func updateRecordFiles(ctx context.Context, recordID string, fileTokens []string) error {
	atts := make([]map[string]string, 0, len(fileTokens))
	for _, ft := range fileTokens {
		atts = append(atts, map[string]string{"file_token": ft})
	}
	fields := map[string]any{
		"Файлы":          atts,
		"ФайлыЗагружены": true,
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
	if err = putRecord(ctx, recordID, fields); err == nil {
		return nil
	}
	// fallback: только вложения, без чекбокса
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

// processFileDownloads — основной проход файлового пайплайна.
func processFileDownloads(ctx context.Context) fileRunResult {
	filesRunMu.Lock()
	defer filesRunMu.Unlock()

	res := fileRunResult{StartedAt: time.Now().Format(time.RFC3339)}
	defer func() { res.FinishedAt = time.Now().Format(time.RFC3339) }()

	recs, err := findRecordsForFiles(ctx)
	if err != nil {
		res.Errors = append(res.Errors, "search: "+err.Error())
		return res
	}
	res.RecordsFound = len(recs)
	log.Printf("[files] карточек «На рассмотрении» без файлов: %d", len(recs))

	for _, rec := range recs {
		number := bitableText(rec.Fields["Номер"])
		// клиентская перестраховка: пропускаем, если файлы уже есть или чекбокс отмечен
		if done, _ := rec.Fields["ФайлыЗагружены"].(bool); done {
			continue
		}
		if atts, ok := rec.Fields["Файлы"].([]any); ok && len(atts) > 0 {
			continue
		}
		tenderID := bitableText(rec.Fields["TenderplanID"])
		if tenderID == "" {
			res.Errors = append(res.Errors, number+": пустой TenderplanID")
			continue
		}

		atts, err := fetchAttachments(ctx, tenderID)
		if err != nil {
			res.Errors = append(res.Errors, number+": attachments "+err.Error())
			continue
		}
		if len(atts) == 0 {
			log.Printf("[files] %s (%s): файлов нет — отмечаем ФайлыЗагружены", number, tenderID)
			if err := updateRecordFiles(ctx, rec.RecordID, nil); err != nil {
				res.Errors = append(res.Errors, number+": mark-empty "+err.Error())
				continue
			}
			res.RecordsDone++
			continue
		}

		var tokens []string
		for _, att := range atts {
			name := sanitizeFileName(att.RealName)
			if att.RealName == "" {
				name = sanitizeFileName(att.DisplayName)
			}
			path, size, err := downloadAttachment(ctx, att)
			if err != nil {
				res.FilesFailed++
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %s download: %v", number, name, err))
				log.Printf("[files] %s: %s — ошибка скачивания: %v", number, name, err)
				continue
			}
			res.FilesDownloaded++
			token, err := uploadFileToDrive(ctx, path, name, size)
			os.Remove(path) // временный файл больше не нужен
			if err != nil {
				res.FilesFailed++
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %s upload: %v", number, name, err))
				log.Printf("[files] %s: %s (%d bytes) — ошибка загрузки в Lark: %v", number, name, size, err)
				continue
			}
			res.FilesUploaded++
			tokens = append(tokens, token)
			log.Printf("[files] %s: %s (%d bytes) — загружен, file_token=%s", number, name, size, token)
			time.Sleep(200 * time.Millisecond)
		}

		if len(tokens) == 0 {
			continue // все файлы упали — карточку не трогаем, повторим в следующем проходе
		}
		if err := updateRecordFiles(ctx, rec.RecordID, tokens); err != nil {
			res.Errors = append(res.Errors, number+": update record "+err.Error())
			continue
		}
		res.RecordsDone++
		log.Printf("[files] %s: карточка обновлена, файлов: %d", number, len(tokens))
	}
	return res
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
		log.Printf("[run] done: processed=%d added=%d duplicates=%d errors=%d %v",
			res.Processed, res.Added, res.Duplicates, len(res.Errors), res.Errors)
	}()
	writeJSON(w, 202, map[string]string{"status": "started", "hint": "смотрите логи контейнера: [run] done: ..."})
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
				log.Printf("[scheduler] done: processed=%d added=%d duplicates=%d errors=%d",
					res.Processed, res.Added, res.Duplicates, len(res.Errors))
			}
		}
	}()
}

type server struct{}

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

	mux := http.NewServeMux()
	h := &server{}
	mux.HandleFunc("/", h.handleRoot)
	mux.HandleFunc("/run", h.handleRun)
	mux.HandleFunc("/files/run", h.handleFilesRun)
	mux.HandleFunc("/webhook/mail-tenders", h.handleMailTenders)
	mux.HandleFunc("/tenders", h.handleListTenders)

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
