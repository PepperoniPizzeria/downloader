package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ========== ساختارهای دیتا ==========

type Update struct {
	UpdateID int      `json:"update_id"`
	Message  *Message `json:"message"`
}

type Message struct {
	Chat Chat   `json:"chat"`
	Text string `json:"text"`
}

type Chat struct {
	ID int64 `json:"id"`
}

type GetUpdatesResponse struct {
	Ok     bool     `json:"ok"`
	Result []Update `json:"result"`
}

// ========== متغیرهای اصلی ==========

var (
	telegramToken string
	ghToken       string
	repoOwner     string
	repoName      string
	offsetFile    = "offset.txt"
	downloadDir   = "downloads"
	baseURL       = "https://tapi.bale.ai/bot"
	githubAPIBase = "https://api.github.com"
	httpClient    = &http.Client{Timeout: 30 * time.Second}
	maxParallel   = 5
)

func main() {
	telegramToken = os.Getenv("TELEGRAM_TOKEN")
	ghToken = os.Getenv("GITHUB_TOKEN")
	repoOwner = os.Getenv("REPO_OWNER")
	repoName = os.Getenv("REPO_NAME")

	if telegramToken == "" || ghToken == "" || repoOwner == "" || repoName == "" {
		log.Fatal("Missing environment variables (TELEGRAM_TOKEN, GITHUB_TOKEN, REPO_OWNER, REPO_NAME)")
	}

	offset := getOffset()
	startTime := time.Now()
	maxDuration := 50 * time.Minute

	log.Printf("Bot started with offset=%d\n", offset)

	for time.Since(startTime) < maxDuration {
		resp, err := httpClient.Get(fmt.Sprintf(
			"%s%s/getUpdates?offset=%d&timeout=30",
			baseURL, telegramToken, offset,
		))
		if err != nil {
			log.Println("getUpdates error:", err)
			time.Sleep(2 * time.Second)
			continue
		}
		var updatesResp GetUpdatesResponse
		json.NewDecoder(resp.Body).Decode(&updatesResp)
		resp.Body.Close()

		if !updatesResp.Ok {
			log.Println("API not ok")
			time.Sleep(5 * time.Second)
			continue
		}

		for _, upd := range updatesResp.Result {
			offset = upd.UpdateID + 1
			saveOffset(offset) // ذخیره فوری offset
			if upd.Message == nil || upd.Message.Text == "" {
				continue
			}
			chatID := upd.Message.Chat.ID
			text := upd.Message.Text

			// پردازش دستورات و حالت‌ها
			handleMessage(chatID, text)
		}
	}
	log.Println("Time limit reached, exiting.")
}

func handleMessage(chatID int64, text string) {
	// تشخیص دستورات خاص
	switch {
	case strings.HasPrefix(text, "/start"):
		sendMessage(chatID, "👋 سلام! لینک‌ها را ارسال کنید.\n/help برای راهنما")
		return
	case strings.HasPrefix(text, "/help"):
		sendMessage(chatID, `📌 راهنما:
/start - شروع
/simple - دانلود همه فایل‌ها به‌صورت عادی
/zipall - همه فایل‌ها در یک فایل zip
/zipeach - هر فایل در یک zip جداگانه
/list - لیست فایل‌های موجود در ریپازیتوری
نکته: اگر دستوری نزنید و یک لینک بدهید، فایل ساده ذخیره می‌شود.
اگر چند لینک بدهید، به‌طور پیش‌فرض همه در یک zip قرار می‌گیرند.`)
		return
	case strings.HasPrefix(text, "/list"):
		handleList(chatID)
		return
	}

	// تشخیص حالت (اگر دستور حالت قبل از لینک‌ها باشد)
	mode := "" // empty = auto
	if strings.HasPrefix(text, "/simple") {
		mode = "simple"
		text = strings.TrimPrefix(text, "/simple")
	} else if strings.HasPrefix(text, "/zipall") {
		mode = "zipall"
		text = strings.TrimPrefix(text, "/zipall")
	} else if strings.HasPrefix(text, "/zipeach") {
		mode = "zipeach"
		text = strings.TrimPrefix(text, "/zipeach")
	}

	// استخراج لینک‌ها
	urls := extractURLs(text)
	if len(urls) == 0 {
		sendMessage(chatID, "❌ لینکی در پیام شما پیدا نشد.")
		return
	}

	// تعیین رفتار بر اساس حالت و تعداد لینک‌ها
	if mode == "" {
		if len(urls) == 1 {
			mode = "simple"
		} else {
			mode = "zipall"
		}
	}

	sendMessage(chatID, "⏳ در حال دانلود...")

	// دانلود فایل‌ها
	filesMap := downloadAll(urls)
	if len(filesMap) == 0 {
		sendMessage(chatID, "❌ هیچ فایلی دانلود نشد.")
		return
	}

	switch mode {
	case "simple":
		commitSimple(chatID, filesMap)
	case "zipall":
		commitZipAll(chatID, filesMap)
	case "zipeach":
		commitZipEach(chatID, filesMap)
	}
}

// ========== عملیات با گیتهاب ==========

func commitFileToRepo(path string, content []byte) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/contents/%s", githubAPIBase, repoOwner, repoName, path)

	// گرفتن sha در صورت وجود فایل
	sha := ""
	resp, err := httpClient.Get(apiURL)
	if err == nil && resp.StatusCode == 200 {
		var existing struct {
			SHA string `json:"sha"`
		}
		json.NewDecoder(resp.Body).Decode(&existing)
		sha = existing.SHA
		resp.Body.Close()
	}

	payload := map[string]interface{}{
		"message": fmt.Sprintf("Add/update %s", path),
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  "main",
	}
	if sha != "" {
		payload["sha"] = sha
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", apiURL, bytes.NewReader(body))
	req.Header.Set("Authorization", "token "+ghToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err = httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func commitSimple(chatID int64, files map[string][]byte) {
	for name, data := range files {
		path := downloadDir + "/" + name
		if err := commitFileToRepo(path, data); err != nil {
			sendMessage(chatID, fmt.Sprintf("❌ خطا در ذخیره %s: %v", name, err))
			continue
		}
		rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/main/%s", repoOwner, repoName, path)
		sendMessage(chatID, fmt.Sprintf("✅ ذخیره شد: %s", rawURL))
	}
}

func commitZipAll(chatID int64, files map[string][]byte) {
	zipData, err := createZipArchive(files)
	if err != nil {
		sendMessage(chatID, "❌ خطا در ساخت zip")
		return
	}
	zipName := fmt.Sprintf("archive_%d.zip", time.Now().Unix())
	path := downloadDir + "/" + zipName
	if err := commitFileToRepo(path, zipData); err != nil {
		sendMessage(chatID, fmt.Sprintf("❌ خطا در ذخیره zip: %v", err))
		return
	}
	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/main/%s", repoOwner, repoName, path)
	sendMessage(chatID, fmt.Sprintf("✅ فایل zip آماده:\n%s", rawURL))
}

func commitZipEach(chatID int64, files map[string][]byte) {
	suffix := time.Now().Unix()
	for name, data := range files {
		archive := map[string][]byte{name: data}
		zipData, err := createZipArchive(archive)
		if err != nil {
			sendMessage(chatID, fmt.Sprintf("❌ خطا در zip کردن %s", name))
			continue
		}
		zipName := fmt.Sprintf("%s_%d.zip", strings.TrimSuffix(name, path.Ext(name)), suffix)
		path := downloadDir + "/" + zipName
		if err := commitFileToRepo(path, zipData); err != nil {
			sendMessage(chatID, fmt.Sprintf("❌ خطا در ذخیره %s: %v", zipName, err))
			continue
		}
		rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/main/%s", repoOwner, repoName, path)
		sendMessage(chatID, fmt.Sprintf("✅ %s", rawURL))
	}
}

// ========== ابزارهای کمکی ==========

func getOffset() int {
	data, err := os.ReadFile(offsetFile)
	if err != nil {
		return 0
	}
	off, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return off
}

func saveOffset(offset int) {
	os.WriteFile(offsetFile, []byte(strconv.Itoa(offset)), 0644)
}

func sendMessage(chatID int64, text string) {
	data := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	b, _ := json.Marshal(data)
	http.Post(baseURL+telegramToken+"/sendMessage", "application/json", bytes.NewReader(b))
}

func extractURLs(text string) []string {
	re := regexp.MustCompile(`https?://[^\s]+`)
	return re.FindAllString(text, -1)
}

func downloadFile(urlStr string) ([]byte, error) {
	req, _ := http.NewRequest("GET", urlStr, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func guessFileName(rawURL string) string {
	parsed, _ := url.Parse(rawURL)
	fname := path.Base(parsed.Path)
	if fname == "" || fname == "." {
		fname = "downloaded_file"
	}
	return fname
}

func downloadAll(urls []string) map[string][]byte {
	files := make(map[string][]byte)
	var mu sync.Mutex
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	for _, u := range urls {
		wg.Add(1)
		go func(dlURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			data, err := downloadFile(dlURL)
			if err != nil {
				log.Printf("Error downloading %s: %v", dlURL, err)
				return
			}
			fname := guessFileName(dlURL)
			mu.Lock()
			files[fname] = data
			mu.Unlock()
		}(u)
	}
	wg.Wait()
	return files
}

func createZipArchive(files map[string][]byte) ([]byte, error) {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)
	for name, data := range files {
		f, err := w.Create(name)
		if err != nil {
			return nil, err
		}
		f.Write(data)
	}
	w.Close()
	return buf.Bytes(), nil
}

func handleList(chatID int64) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/contents/%s", githubAPIBase, repoOwner, repoName, downloadDir)
	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("Authorization", "token "+ghToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		sendMessage(chatID, "❌ خطا در دریافت لیست.")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		sendMessage(chatID, "📭 پوشه‌ی دانلود خالی یا در دسترس نیست.")
		return
	}
	var items []struct {
		Name        string `json:"name"`
		DownloadURL string `json:"download_url"`
		Type        string `json:"type"`
	}
	json.NewDecoder(resp.Body).Decode(&items)
	if len(items) == 0 {
		sendMessage(chatID, "هنوز فایلی آپلود نشده.")
		return
	}
	var msg strings.Builder
	msg.WriteString("📂 فایل‌های موجود:\n")
	for _, item := range items {
		if item.Type == "file" {
			msg.WriteString(fmt.Sprintf("• [%s](%s)\n", item.Name, item.DownloadURL))
		}
	}
	sendMessage(chatID, msg.String())
}
