// bot.go
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

// =========== ساختارهای تلگرام و گیتهاب ===========

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

type GitHubContent struct {
	Content string `json:"content"`
	Message string `json:"message"`
	Branch  string `json:"branch"`
}

// =========== متغیرهای سراسری ===========

var (
	telegramToken  string
	ghToken        string
	repoOwner      string
	repoName       string
	offsetFile     = "offset.txt"
	downloadDir    = "downloads"
	baseURL        = "https://api.telegram.org/bot"
	githubAPIBase  = "https://api.github.com"
	httpClient     = &http.Client{Timeout: 30 * time.Second}
	maxParallel    = 5  // حداکثر دانلود هم‌زمان
	useCompression = false // پرچم فشرده‌سازی برای پیام فعلی
)

// =========== ابزارهای کمکی ===========

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

// commitFileToRepo یک فایل را با API گیتهاب ذخیره می‌کند
func commitFileToRepo(filePath string, content []byte) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/contents/%s", githubAPIBase, repoOwner, repoName, filePath)

	// بررسی وجود فایل (برای گرفتن sha)
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

	payload := GitHubContent{
		Content: base64.StdEncoding.EncodeToString(content),
		Message: fmt.Sprintf("Add/update %s", filePath),
		Branch:  "main", // یا نام شاخه پیش‌فرض
	}
	// if sha exists, add to payload
	bodyBytes, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", apiURL, bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "token "+ghToken)
	req.Header.Set("Content-Type", "application/json")
	// اضافه کردن sha به صورت query parameter? Actually GitHub API expects "sha" in body for updates.
	// If we have sha, we must include it. Redefine payload as map.
	if sha != "" {
		var m map[string]interface{}
		json.Unmarshal(bodyBytes, &m)
		m["sha"] = sha
		bodyBytes, _ = json.Marshal(m)
		req, _ = http.NewRequest("PUT", apiURL, bytes.NewReader(bodyBytes))
		req.Header.Set("Authorization", "token "+ghToken)
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err = httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// sendMessage ارسال پیام به کاربر
func sendMessage(chatID int64, text string) {
	data := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	b, _ := json.Marshal(data)
	http.Post(baseURL+telegramToken+"/sendMessage", "application/json", bytes.NewReader(b))
}

// extractURLs استخراج لینک‌ها از متن
func extractURLs(text string) []string {
	re := regexp.MustCompile(`https?://[^\s]+`)
	return re.FindAllString(text, -1)
}

// guessFileName حدس نام فایل از URL یا هدر Content-Disposition
func guessFileName(rawURL string) string {
	parsed, _ := url.Parse(rawURL)
	fname := path.Base(parsed.Path)
	if fname == "" || fname == "." || fname == "/" {
		fname = "downloaded_file"
	}
	// سعی در دریافت هدر
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Head(rawURL)
	if err == nil {
		cd := resp.Header.Get("Content-Disposition")
		if cd != "" {
			re := regexp.MustCompile(`filename\*?=(?:UTF-8'')?["']?([^"'; \s]+)`)
			match := re.FindStringSubmatch(cd)
			if len(match) > 1 {
				fname = match[1]
			}
		}
	}
	return fname
}

// downloadFile فایل را با دنبال کردن ۱۰ تغییر مسیر دانلود می‌کند
func downloadFile(urlStr string) ([]byte, error) {
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// createZipArchive فایل‌های داده‌شده را به یک فایل zip تبدیل می‌کند
func createZipArchive(files map[string][]byte) ([]byte, error) {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)
	for name, data := range files {
		f, err := w.Create(name)
		if err != nil {
			return nil, err
		}
		_, err = f.Write(data)
		if err != nil {
			return nil, err
		}
	}
	w.Close()
	return buf.Bytes(), nil
}

// =========== دستورات ===========

func handleList(chatID int64) {
	// دریافت لیست فایل‌ها از API گیتهاب (محتوای پوشه downloads)
	apiURL := fmt.Sprintf("%s/repos/%s/%s/contents/%s", githubAPIBase, repoOwner, repoName, downloadDir)
	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("Authorization", "token "+ghToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		sendMessage(chatID, "خطا در دریافت لیست فایل‌ها.")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		sendMessage(chatID, "پوشه‌ی دانلود خالی است یا خطا رخ داد.")
		return
	}
	var items []struct {
		Name        string `json:"name"`
		DownloadURL string `json:"download_url"`
		Type        string `json:"type"`
	}
	json.NewDecoder(resp.Body).Decode(&items)
	if len(items) == 0 {
		sendMessage(chatID, "هنوز فایلی دانلود نشده است.")
		return
	}
	var msg strings.Builder
	msg.WriteString("📂 فایل‌های موجود:\n")
	for _, item := range items {
		if item.Type == "file" {
			msg.WriteString(fmt.Sprintf("- [%s](%s)\n", item.Name, item.DownloadURL))
		}
	}
	sendMessage(chatID, msg.String())
}

// =========== هسته اصلی ===========

func main() {
	telegramToken = os.Getenv("TELEGRAM_TOKEN")
	ghToken = os.Getenv("GH_PAT")
	repoOwner = os.Getenv("REPO_OWNER")
	repoName = os.Getenv("REPO_NAME")

	if telegramToken == "" || ghToken == "" || repoOwner == "" || repoName == "" {
		log.Fatal("Missing environment variables")
	}

	offset := getOffset()
	startTime := time.Now()
	maxDuration := 50 * time.Minute // کمی کمتر از ۵۵ دقیقه

	log.Printf("ربات با offset=%d شروع به کار کرد.\n", offset)

	for time.Since(startTime) < maxDuration {
		// long poll
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
			saveOffset(offset) // ذخیره برای بعد
			if upd.Message == nil || upd.Message.Text == "" {
				continue
			}
			chatID := upd.Message.Chat.ID
			text := upd.Message.Text

			// مدیریت دستورات
			switch {
			case strings.HasPrefix(text, "/start"):
				sendMessage(chatID, "سلام! لینک(ها) را بفرستید تا دانلود کنم.\nبرای فشرده‌سازی از /zip استفاده کنید.\n/help برای راهنما.")
			case strings.HasPrefix(text, "/help"):
				sendMessage(chatID, "📌 راهنما:\n- ارسال لینک → دانلود مستقیم\n- چند لینک → فایل zip\n- /zip قبل از لینک‌ها → فشرده‌سازی اجباری\n- /list → لیست فایل‌های موجود")
			case strings.HasPrefix(text, "/list"):
				handleList(chatID)
			default:
				// چک کردن flag فشرده‌سازی
				forceZip := false
				msgText := text
				if strings.HasPrefix(strings.ToLower(text), "/zip") {
					forceZip = true
					msgText = strings.TrimSpace(strings.TrimPrefix(text, "/zip"))
				}

				urls := extractURLs(msgText)
				if len(urls) == 0 {
					sendMessage(chatID, "لینکی پیدا نشد.")
					continue
				}

				sendMessage(chatID, "⏳ در حال دانلود...")

				filesMap := make(map[string][]byte)
				var mu sync.Mutex
				sem := make(chan struct{}, maxParallel)
				var wg sync.WaitGroup
				var dlErrors []string

				for _, u := range urls {
					wg.Add(1)
					go func(dlURL string) {
						defer wg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()

						data, err := downloadFile(dlURL)
						if err != nil {
							mu.Lock()
							dlErrors = append(dlErrors, fmt.Sprintf("%s: %v", dlURL, err))
							mu.Unlock()
							return
						}
						fname := guessFileName(dlURL)
						mu.Lock()
						filesMap[fname] = data
						mu.Unlock()
					}(u)
				}
				wg.Wait()

				if len(filesMap) == 0 {
					sendMessage(chatID, fmt.Sprintf("❌ دانلود ناموفق بود:\n%s", strings.Join(dlErrors, "\n")))
					continue
				}

				// تصمیم‌گیری برای zip
				if len(filesMap) > 1 || forceZip {
					zipData, err := createZipArchive(filesMap)
					if err != nil {
						sendMessage(chatID, "خطا در ساخت فایل zip")
						continue
					}
					zipName := fmt.Sprintf("archive_%d.zip", time.Now().Unix())
					commitPath := downloadDir + "/" + zipName
					err = commitFileToRepo(commitPath, zipData)
					if err != nil {
						sendMessage(chatID, "❌ خطا در ذخیره‌سازی در گیتهاب: "+err.Error())
					} else {
						rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/main/%s", repoOwner, repoName, commitPath)
						sendMessage(chatID, fmt.Sprintf("✅ فایل فشرده آماده شد:\n%s", rawURL))
					}
				} else {
					// تک فایل
					for name, data := range filesMap {
						commitPath := downloadDir + "/" + name
						err := commitFileToRepo(commitPath, data)
						if err != nil {
							sendMessage(chatID, "❌ خطا در ذخیره‌سازی: "+err.Error())
						} else {
							rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/main/%s", repoOwner, repoName, commitPath)
							sendMessage(chatID, fmt.Sprintf("✅ دانلود شد:\n%s", rawURL))
						}
						break // فقط یکی
					}
				}
			}
		}
	}
	log.Println("زمان اجرا تمام شد، خروج...")
}