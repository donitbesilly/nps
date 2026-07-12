// Package translate optionally localizes foreign province/city names to
// Chinese using an OpenAI-chat-completions-compatible LLM API (works with
// OpenAI, Anthropic's compatible endpoint, or any compatible gateway).
//
// The feature is entirely opt-in: Init is a no-op unless both an API URL
// and API key are configured, so a default install makes zero network
// calls. Translation never blocks a tunnel connection -- callers schedule
// a place name with Enqueue, which is processed by a background worker and
// cached forever (by exact country/province/city text) so a given place is
// only ever sent to the API once.
package translate

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	apiURL  string
	apiKey  string
	model   string
	enabled bool

	db      *sql.DB
	dbMu    sync.Mutex
	pending sync.Map // "country|province|city" -> struct{}{}, in-flight dedupe
	queue   chan job
)

type job struct {
	Country, Province, City string
}

// Init configures the translator. Safe to call once at startup; a blank
// apiURL or apiKey leaves the feature disabled (Enqueue becomes a no-op).
func Init(url, key, modelName, dbPath string) error {
	apiURL, apiKey, model = strings.TrimSpace(url), strings.TrimSpace(key), strings.TrimSpace(modelName)
	enabled = apiURL != "" && apiKey != ""
	if model == "" {
		model = "gpt-4o-mini"
	}
	if !enabled {
		return nil
	}
	conn, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS place_translations (
			country TEXT NOT NULL,
			province TEXT NOT NULL,
			city TEXT NOT NULL,
			province_zh TEXT,
			city_zh TEXT,
			PRIMARY KEY (country, province, city)
		)
	`); err != nil {
		conn.Close()
		return err
	}
	db = conn
	queue = make(chan job, 500)
	go worker()
	return nil
}

// Lookup returns cached Chinese translations for a place, if any have been
// resolved already. It never calls the API itself and never blocks on
// network I/O.
func Lookup(country, province, city string) (provinceZH, cityZH string, ok bool) {
	if db == nil || (province == "" && city == "") {
		return "", "", false
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	var pzh, czh sql.NullString
	err := db.QueryRow(
		`SELECT province_zh, city_zh FROM place_translations WHERE country = ? AND province = ? AND city = ?`,
		country, province, city,
	).Scan(&pzh, &czh)
	if err != nil {
		return "", "", false
	}
	return pzh.String, czh.String, pzh.Valid || czh.Valid
}

// Enqueue schedules a background translation for a place if it hasn't been
// translated (or isn't already queued) yet. Always non-blocking: if the
// queue is full the request is simply dropped, since this is a best-effort
// display enhancement, not something a connection should ever wait on.
func Enqueue(country, province, city string) {
	if !enabled || (province == "" && city == "") {
		return
	}
	key := country + "|" + province + "|" + city
	if _, loaded := pending.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	if _, _, ok := Lookup(country, province, city); ok {
		pending.Delete(key)
		return
	}
	select {
	case queue <- job{Country: country, Province: province, City: city}:
	default:
		pending.Delete(key)
	}
}

func worker() {
	for j := range queue {
		provinceZH, cityZH, err := callLLM(j.Country, j.Province, j.City)
		if err == nil {
			saveCache(j.Country, j.Province, j.City, provinceZH, cityZH)
		}
		pending.Delete(j.Country + "|" + j.Province + "|" + j.City)
	}
}

func saveCache(country, province, city, provinceZH, cityZH string) {
	dbMu.Lock()
	defer dbMu.Unlock()
	db.Exec(
		`INSERT INTO place_translations (country, province, city, province_zh, city_zh) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(country, province, city) DO UPDATE SET province_zh=excluded.province_zh, city_zh=excluded.city_zh`,
		country, province, city, provinceZH, cityZH,
	)
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

type placeTranslation struct {
	Province string `json:"province"`
	City     string `json:"city"`
}

func callLLM(country, province, city string) (provinceZH, cityZH string, err error) {
	prompt := fmt.Sprintf(
		"将下面的地名翻译成简体中文的常见译名，只返回一个JSON对象，不要任何解释或代码块标记。"+
			"国家：%s；省/州：%s；城市：%s。返回格式：{\"province\":\"...\",\"city\":\"...\"}（如果某一项是空字符串，对应译名也返回空字符串）",
		country, province, city,
	)
	reqBody, err := json.Marshal(chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return "", "", err
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("translate api status %d: %s", resp.StatusCode, string(body))
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil || len(cr.Choices) == 0 {
		return "", "", fmt.Errorf("translate api: unexpected response: %s", string(body))
	}
	content := strings.TrimSpace(cr.Choices[0].Message.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var pt placeTranslation
	if err := json.Unmarshal([]byte(content), &pt); err != nil {
		return "", "", fmt.Errorf("translate api: could not parse place JSON %q: %w", content, err)
	}
	return pt.Province, pt.City, nil
}
