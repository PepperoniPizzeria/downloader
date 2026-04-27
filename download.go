// download.go
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	triggerFile = "trigger_download.txt"
	downloadDir = "downloads"
	httpClient  = &http.Client{Timeout: 60 * time.Second}
	maxParallel = 5
)

func main() {
	data, err := os.ReadFile(triggerFile)
	if err != nil {
		log.Fatal("cannot read trigger file")
	}
	lines := strings.Split(string(data), "\n")
	var urls []string
	forceZip := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// تشخیص flag فشرده‌سازی
		if strings.ToLower(line) == "/zip" {
			forceZip = true
			continue
		}
		re := regexp.MustCompile(`https?://[^\s]+`)
		found := re.FindAllString(line, -1)
		urls = append(urls, found...)
	}
	if len(urls) == 0 {
		log.Println("No URLs found. Exiting.")
		return
	}

	os.MkdirAll(downloadDir, 0755)

	filesMap := make(map[string][]byte)
	var mu sync.Mutex
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	for _, u := range urls {
		wg.Add(1)
		go func(dlURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req, _ := http.NewRequest("GET", dlURL, nil)
			resp, err := httpClient.Do(req)
			if err != nil {
				log.Printf("download error %s: %v\n", dlURL, err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				log.Printf("bad status %s: %d\n", dlURL, resp.StatusCode)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			fname := guessFileName(dlURL)
			mu.Lock()
			filesMap[fname] = body
			mu.Unlock()
		}(u)
	}
	wg.Wait()

	if len(filesMap) == 0 {
		log.Fatal("No files downloaded.")
	}

	// اگر چند فایل بود یا flag بود، zip کن
	if len(filesMap) > 1 || forceZip {
		zipName := fmt.Sprintf("archive_%d.zip", time.Now().Unix())
		zipPath := filepath.Join(downloadDir, zipName)
		zipFile, err := os.Create(zipPath)
		if err != nil {
			log.Fatal(err)
		}
		defer zipFile.Close()
		w := zip.NewWriter(zipFile)
		for name, content := range filesMap {
			f, _ := w.Create(name)
			f.Write(content)
		}
		w.Close()
	} else {
		// تک فایل
		for name, content := range filesMap {
			outPath := filepath.Join(downloadDir, name)
			os.WriteFile(outPath, content, 0644)
			break
		}
	}

	log.Println("Download completed. Files saved.")
}

func guessFileName(rawURL string) string {
	parsed, _ := url.Parse(rawURL)
	fname := path.Base(parsed.Path)
	if fname == "" || fname == "." {
		fname = "downloaded_file"
	}
	return fname
}