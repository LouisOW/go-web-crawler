package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gorilla/websocket"
)

type PageInfo struct {
	URL                string
	Title              string
	StatusCode         int
	LoadTime           time.Duration
	SelfReferencingURL bool
	AnchorDetails      string
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	t, _ := template.ParseFiles("templates/upload.html")
	t.Execute(w, nil)
}

// List of classes to ignore
var ignoredClasses = map[string]bool{
	"footer__copy-logo":                  true,
	"header__upper-link":                 true,
	"mp-share__toggle":                   true,
	"mp-share__toggle  breadcrumbs__tag": true,
	"mp-link mp-link--dark localisation-toggle localisation-setter": true,
	"hash-scroll":                       true,
	"mp-available-session__show-detail": true,
	"mp-available-session__hide-detail": true,
	// Add more classes as needed
}

// Function to check for self-referencing links with href="#" and ignore specified classes
func checkSelfReferencingLinks(doc *goquery.Document) (bool, string) {
	found := false
	var anchorDetails []string

	doc.Find("a").Each(func(i int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		class, _ := s.Attr("class")

		// Check if the href is exactly "#" and class is not in ignored classes
		if href == "#" && !ignoredClasses[class] {
			title := s.AttrOr("title", "No title")
			anchor := fmt.Sprintf("<a href=\"%s\" class=\"%s\" title=\"%s\">", href, class, title)
			anchorDetails = append(anchorDetails, anchor)
			found = true
			fmt.Printf("Self-referencing link found (not ignored class):\n")
			fmt.Printf("URL: %s\n", href)
			fmt.Printf("Class: %s\n", class)
			fmt.Printf("Title: %s\n", title)
			fmt.Println("-----")
		}
	})

	return found, strings.Join(anchorDetails, ",")
}

func removeBOM(s string) string {
	bom := []byte{0xEF, 0xBB, 0xBF}
	if bytes.HasPrefix([]byte(s), bom) {
		return string(bytes.TrimPrefix([]byte(s), bom))
	}
	return s
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "Could not open websocket connection", http.StatusBadRequest)
		return
	}
	defer conn.Close()

	_, msg, err := conn.ReadMessage()
	if err != nil {
		fmt.Println("Error reading message:", err)
		return
	}

	file, err := os.CreateTemp("", "upload-*.csv")
	if err != nil {
		fmt.Println("Error creating temp file:", err)
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()

	if _, err := file.Write(msg); err != nil {
		fmt.Println("Error writing to temp file:", err)
		return
	}

	file.Seek(0, 0)
	reader := csv.NewReader(file)

	var pageInfos []PageInfo
	var urls []string

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Println("Error reading CSV file:", err)
			conn.WriteMessage(websocket.TextMessage, []byte("Error reading CSV file"))
			return
		}

		url := removeBOM(strings.TrimSpace(record[0]))
		if url == "" {
			fmt.Println("Empty URL found, skipping.")
			continue
		}

		fmt.Printf("Processing URL: %s\n", url)
		urls = append(urls, url)
	}

	totalUrls := len(urls)
	for i, url := range urls {
		conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Processing: %s", url)))

		start := time.Now()
		resp, err := http.Get(url)
		if err != nil {
			pageInfos = append(pageInfos, PageInfo{URL: url, Title: "Error", StatusCode: 0, LoadTime: 0})
			fmt.Printf("Error fetching URL: %s, error: %v\n", url, err)
			continue
		}
		defer resp.Body.Close()

		loadTime := time.Since(start)
		doc, err := goquery.NewDocumentFromReader(resp.Body)
		if err != nil {
			pageInfos = append(pageInfos, PageInfo{URL: url, Title: "Error", StatusCode: resp.StatusCode, LoadTime: loadTime})
			fmt.Printf("Error parsing HTML for URL: %s, error: %v\n", url, err)
			continue
		}

		title := doc.Find("title").Text()
		selfReferencingURL, anchorDetails := checkSelfReferencingLinks(doc)
		pageInfos = append(pageInfos, PageInfo{
			URL:                url,
			Title:              title,
			StatusCode:         resp.StatusCode,
			LoadTime:           loadTime,
			SelfReferencingURL: selfReferencingURL,
			AnchorDetails:      anchorDetails,
		})

		progress := int(float64(i+1) / float64(totalUrls) * 100)
		conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Progress: %d%%", progress)))
	}

	outputFileName := "output.csv"
	outputFile, err := os.Create(outputFileName)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte("Error creating output file"))
		return
	}
	defer outputFile.Close()

	writer := csv.NewWriter(outputFile)
	defer writer.Flush()

	writer.Write([]string{"URL", "Title", "Status Code", "Load Time (ms)", "Self-Referencing URL with #", "Anchor Details"})
	for _, pageInfo := range pageInfos {
		writer.Write([]string{
			pageInfo.URL,
			pageInfo.Title,
			fmt.Sprintf("%d", pageInfo.StatusCode),
			fmt.Sprintf("%d", pageInfo.LoadTime.Milliseconds()),
			fmt.Sprintf("%t", pageInfo.SelfReferencingURL),
			pageInfo.AnchorDetails,
		})
	}

	conn.WriteMessage(websocket.TextMessage, []byte("Processing completed"))
	conn.WriteMessage(websocket.TextMessage, []byte("Download link: /download/output.csv"))
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "output.csv")
}

func main() {
	http.HandleFunc("/", uploadHandler)
	http.HandleFunc("/upload", wsHandler)
	http.HandleFunc("/download/output.csv", downloadHandler)

	// Serve static files
	fs := http.FileServer(http.Dir("static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
		fmt.Println("No PORT environment variable detected, defaulting to " + port)
	}

	fmt.Println("Server started at :" + port)
	http.ListenAndServe(":"+port, nil)
}
