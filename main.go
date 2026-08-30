// Package main — Tender Monitor: ежедневный мониторинг тендеров Tenderplan + Lark Bitable + Kimi-резюме.
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
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ===== CONFIG =====

const (
	tenderplanAPI   = "https://tenderplan.ru/api"
	kimiAPI         = "https://api.moonshot.ai/v1/chat/completions"
	kimiModel       = "kimi-k2.6"
	larkBase        = "https://open.larksuite.com/open-apis"
	listenAddr      = ":8787"
	dailyRunHour    = 8
	dailyRunMinute  = 0
	defaultAppID    = "cli_aa1810fe6f78c079"
	defaultBitableApp = "X37KbBltZaqSSdsGyJdumNLFtmh"
	defaultBitableTable = "tblalXw0gAIi3pVc"
	defaultChatID   = "oc_6cc3a4c2e69b74e6a7d240c1e95db951"
	tokenTTL        = 2 * time.Hour
)

var (
	tenderplanKey = os.Getenv("TENDERPLAN_KEY")
	kimiKey       = os.Getenv("KIMI_KEY")
	mailSecret    = os.Getenv("MAIL_SECRET")
	larkAppID     = envOr("LARK_APP_ID", defaultAppID)
	larkAppSecret = os.Getenv("LARK_APP_SECRET")
	bitableApp    = envOr("LARK_BITABLE_APP", defaultBitableApp)
	bitableTable  = envOr("LARK_BITABLE_TABLE", defaultBitableTable)
	chatID        = envOr("LARK_CHAT_ID", defaultChatID)
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var clusters = map[string][]string{
	"Росморпорт":   {"Росморпорт", "морской порт", "акватория", "дноуглубление", "берегоукрепление", "гидротехника", "причал"},
	"Малый техфлот": {"земснаряд", "буксир", "шаланда", "понтон", "катер", "моторная яхта", "маломерное судно"},
	"Иное":         {"дноуглубление", "дноуглубительные работы", "судостроение", "гидротехника", "аренда флота"},
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
	resp, err := http.DefaultClient.Do(req)
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
	tkCache.mu.Lock()
	tkCache.value = out.TenantAccessToken
	tkCache.exp = time.Now().Add(tokenTTL)
	tkCache.mu.Unlock()
	return out.TenantAccessToken, nil
}

// ===== TENDERPLAN =====

type Tender struct {
	Number      string  `json:"number"`
	Name        string  `json:"name"`
	Customer    string  `json:"customer"`
	Region      string  `json:"region"`
	Amount      float64 `json:"amount"`
	Deadline    string  `json:"deadline"`
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
}

func fetchTenders(ctx context.Context, clusterName string, keywords []string, since string) ([]Tender, error) {
	q := strings.Join(keywords, " OR ")
	u := tenderplanAPI + "/tenders/getlist?key=" + tenderplanKey + "&q=" + urlQueryEscape(q)
	if since != "" {
		u += "&since=" + urlQueryEscape(since)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data []Tender `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func urlQueryEscape(s string) string {
	// минимальный escape для совместимости с Python OR
	r := strings.NewReplacer(" ", "%20", "/", "%2F", "?", "%3F", "&", "%26", "=", "%3D", "#", "%23")
	return r.Replace(s)
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

	body, _ := json.Marshal(map[string]any{
		"model":       kimiModel,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"temperature": 0.3,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", kimiAPI, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+kimiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
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
	if len(out.Choices) == 0 {
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

func addToBitable(ctx context.Context, t Tender, cluster, summary string) (bitableResult, error) {
	token, err := getTenantToken(ctx)
	if err != nil {
		return bitableResult{}, err
	}

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

	// duplicate check
	dupURL := fmt.Sprintf("%s/bitable/v1/apps/%s/tables/%s/records/search",
		larkBase, bitableApp, bitableTable)
	dupBody, _ := json.Marshal(map[string]any{
		"filter": map[string]any{"and": []any{map[string]any{"field_name": "Номер", "operator": "is", "value": []string{t.Number}}}},
	})
	dupReq, _ := http.NewRequestWithContext(ctx, "POST", dupURL, bytes.NewReader(dupBody))
	dupReq.Header.Set("Authorization", "Bearer "+token)
	dupReq.Header.Set("Content-Type", "application/json")
	dupResp, err := http.DefaultClient.Do(dupReq)
	if err != nil {
		return bitableResult{}, err
	}
	defer dupResp.Body.Close()
	var dupOut struct {
		Data struct {
			Items []json.RawMessage `json:"items"`
		} `json:"data"`
	}
	_ = json.NewDecoder(dupResp.Body).Decode(&dupOut)
	if len(dupOut.Data.Items) > 0 {
		return bitableResult{Status: "duplicate", Record: dupOut.Data.Items[0]}, nil
	}

	// create
	createURL := fmt.Sprintf("%s/bitable/v1/apps/%s/tables/%s/records",
		larkBase, bitableApp, bitableTable)
	createBody, _ := json.Marshal(map[string]any{"fields": fields})
	createReq, _ := http.NewRequestWithContext(ctx, "POST", createURL, bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		return bitableResult{}, err
	}
	defer createResp.Body.Close()
	body, _ := io.ReadAll(createResp.Body)
	var result map[string]any
	_ = json.Unmarshal(body, &result)
	return bitableResult{Code: createResp.StatusCode, Record: result}, nil
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
	url := larkBase + "/im/v1/messages?receive_id_type=chat_id"
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
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
			summary, err := kimiSummarize(ctx, t)
			if err != nil {
				res.Errors = append(res.Errors, t.Number+": kimi "+err.Error())
				continue
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
			}
		}
	}
	return res
}

func processMailTenders(ctx context.Context, tenders []Tender) runResults {
	res := runResults{}
	for _, t := range tenders {
		cluster := classifyCluster(t)
		summary, err := kimiSummarize(ctx, t)
		if err != nil {
			res.Errors = append(res.Errors, t.Number+": kimi "+err.Error())
			continue
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
	writeJSON(w, 200, map[string]string{"status": "ok", "service": "tender-monitor"})
}

func (h *server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	res := processTenders(r.Context())
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
	url := fmt.Sprintf("%s/bitable/v1/apps/%s/tables/%s/records/search", larkBase, bitableApp, bitableTable)
	bodyMap := map[string]any{}
	if status != "" {
		bodyMap["filter"] = map[string]any{"and": []any{map[string]any{"field_name": "Статус", "operator": "is", "value": []string{status}}}}
	}
	body, _ := json.Marshal(bodyMap)
	req, _ := http.NewRequestWithContext(r.Context(), "POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
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

	mux := http.NewServeMux()
	h := &server{}
	mux.HandleFunc("/", h.handleRoot)
	mux.HandleFunc("/run", h.handleRun)
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
