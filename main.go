package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	thumbnails "github.com/drummonds/go-thumbnails"
	"github.com/drummonds/godocs-inbox/internal/llm"
	"github.com/drummonds/godocs-inbox/internal/ocr"
	"gopkg.in/yaml.v3"
)

//go:embed templates/*.html
var templateFS embed.FS

const configFileName = "godocs-inbox.yaml"

// --- Config ---

type ShortcutConfig struct {
	Key   string `yaml:"key"   json:"key"`
	TagID int    `yaml:"tag_id" json:"tag_id"`
	Name  string `yaml:"-"     json:"name"`  // populated from server
	Color string `yaml:"-"     json:"color"` // populated from server
}

type Config struct {
	GodocsServer string           `yaml:"godocs_server"`
	Addr         string           `yaml:"addr"`
	Shortcuts    []ShortcutConfig `yaml:"tags"` // yaml key kept as "tags" for simplicity
	OllamaURL    string           `yaml:"ollama_url,omitempty"`
	OllamaModel  string           `yaml:"ollama_model,omitempty"`
	// Demo-only fields (not in yaml)
	InboxDir  string `yaml:"inbox_dir,omitempty"`
	TaggedDir string `yaml:"tagged_dir,omitempty"`
}

type TagSetEntry struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type RecentTagSet struct {
	Tags  []TagSetEntry
	Label string
}

// --- Godocs API types ---

type GodocsTag struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	TagGroup  string `json:"tag_group"`
	SortOrder int    `json:"sort_order"`
}

type GodocsDocument struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	Folder       string `json:"folder"`
	ULID         string `json:"ulid"`
	DocumentType string `json:"document_type"`
	FullText     string `json:"full_text"`
	IngressTime  string `json:"ingress_time"`
	URL          string `json:"url"`
}

type GodocsSearchResponse struct {
	Documents   []GodocsDocument `json:"documents"`
	Page        int              `json:"page"`
	PageSize    int              `json:"pageSize"`
	TotalCount  int              `json:"totalCount"`
	TotalPages  int              `json:"totalPages"`
	HasNext     bool             `json:"hasNext"`
	HasPrevious bool             `json:"hasPrevious"`
}

type GodocsDocStatus struct {
	ULID          string `json:"ulid"`
	Name          string `json:"name"`
	Path          string `json:"path"`
	DocumentType  string `json:"documentType"`
	HasThumbnail  bool   `json:"hasThumbnail"`
	ThumbnailURL  string `json:"thumbnailURL"`
	HasText       bool   `json:"hasText"`
	TextLength    int    `json:"textLength"`
	TextURL       string `json:"textURL"`
	ViewURL       string `json:"viewURL"`
	IngressTime   string `json:"ingressTime"`
	FileExists    bool   `json:"fileExists"`
	FileSizeBytes int    `json:"fileSizeBytes"`
	TagCount      int    `json:"tagCount"`
	DocumentDate  string `json:"documentDate"`
}

// --- Godocs client ---

type GodocsClient struct {
	baseURL    string
	httpClient *http.Client
	tags       map[int]GodocsTag // tag ID → tag
}

func NewGodocsClient(baseURL string) *GodocsClient {
	return &GodocsClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		tags:       make(map[int]GodocsTag),
	}
}

func (c *GodocsClient) FetchTags() ([]GodocsTag, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/api/tags")
	if err != nil {
		return nil, fmt.Errorf("fetching tags: %w", err)
	}
	defer resp.Body.Close()
	var tags []GodocsTag
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, fmt.Errorf("decoding tags: %w", err)
	}
	c.tags = make(map[int]GodocsTag)
	for _, t := range tags {
		c.tags[t.ID] = t
	}
	return tags, nil
}

func (c *GodocsClient) FetchUntagged(page, pageSize int) (*GodocsSearchResponse, error) {
	url := fmt.Sprintf("%s/api/documents/untagged?page=%d&pageSize=%d", c.baseURL, page, pageSize)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching untagged: %w", err)
	}
	defer resp.Body.Close()
	var sr GodocsSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, fmt.Errorf("decoding untagged: %w", err)
	}
	return &sr, nil
}

func (c *GodocsClient) FetchDocStatus(ulid string) (*GodocsDocStatus, error) {
	url := fmt.Sprintf("%s/api/document/%s/status", c.baseURL, ulid)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching doc status: %w", err)
	}
	defer resp.Body.Close()
	var ds GodocsDocStatus
	if err := json.NewDecoder(resp.Body).Decode(&ds); err != nil {
		return nil, fmt.Errorf("decoding doc status: %w", err)
	}
	return &ds, nil
}

func (c *GodocsClient) FetchDocText(ulid string) (string, error) {
	url := fmt.Sprintf("%s/api/document/%s/text", c.baseURL, ulid)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("fetching doc text: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return "", nil
	}
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding doc text: %w", err)
	}
	if text, ok := result["text"]; ok {
		return text, nil
	}
	return "", nil
}

func (c *GodocsClient) AddTag(ulid string, tagID int) error {
	body := strings.NewReader(fmt.Sprintf(`{"tag_id":%d}`, tagID))
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/api/documents/%s/tags", c.baseURL, ulid), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("adding tag: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("add tag failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *GodocsClient) FetchDocTags(ulid string) ([]GodocsTag, error) {
	url := fmt.Sprintf("%s/api/documents/%s/tags", c.baseURL, ulid)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching doc tags: %w", err)
	}
	defer resp.Body.Close()
	var tags []GodocsTag
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		// godocs returns null for no tags
		return nil, nil
	}
	return tags, nil
}

func (c *GodocsClient) RemoveTag(ulid string, tagID int) error {
	req, err := http.NewRequest("DELETE", fmt.Sprintf("%s/api/documents/%s/tags/%d", c.baseURL, ulid, tagID), nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("removing tag: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("remove tag failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *GodocsClient) FetchTagGroups() ([]string, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/api/tags/groups")
	if err != nil {
		return nil, fmt.Errorf("fetching tag groups: %w", err)
	}
	defer resp.Body.Close()
	var groups []string
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		return nil, nil
	}
	return groups, nil
}

func (c *GodocsClient) DownloadDocument(ulid string) ([]byte, string, error) {
	url := fmt.Sprintf("%s/document/view/%s", c.baseURL, ulid)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, "", fmt.Errorf("downloading document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("download failed with status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("reading document body: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	return body, contentType, nil
}

func (c *GodocsClient) UploadDocumentText(ulid, text string) error {
	body := strings.NewReader(fmt.Sprintf(`{"text":%s}`, jsonString(text)))
	req, err := http.NewRequest("PUT", fmt.Sprintf("%s/api/document/%s/text", c.baseURL, ulid), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("uploading text: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload text failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *GodocsClient) UpdateDocumentDate(ulid, date string) error {
	body := strings.NewReader(fmt.Sprintf(`{"date":"%s"}`, date))
	req, err := http.NewRequest("PUT", fmt.Sprintf("%s/api/document/%s/date", c.baseURL, ulid), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("updating date: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update date failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (c *GodocsClient) CreateTag(name, color, group string) (*GodocsTag, error) {
	payload := map[string]interface{}{
		"name":  name,
		"color": color,
	}
	if group != "" {
		payload["tag_group"] = group
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", c.baseURL+"/api/tags", strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("creating tag: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create tag failed (%d): %s", resp.StatusCode, string(body))
	}
	var tag GodocsTag
	if err := json.NewDecoder(resp.Body).Decode(&tag); err != nil {
		return nil, fmt.Errorf("decoding created tag: %w", err)
	}
	// Update local cache
	c.tags[tag.ID] = tag
	return &tag, nil
}

// --- Processing stages ---

const (
	stageOCR = "ocr"
	stageLLM = "llm"
)

// --- App ---

type LastAction struct {
	DocULID string
	DocName string
	TagID   int
	TagName string
	// Demo mode only
	File    string
	FromDir string
	ToDir   string
}

type App struct {
	mu           sync.Mutex
	config       Config
	configFile   string
	client       *GodocsClient // nil in demo mode
	lastAction   *LastAction
	llmDates     map[string]bool   // ULID → date was set by LLM
	recentSets   []RecentTagSet    // last N applied tag sets
	docStage     map[string]string // ULID → current processing stage (stageOCR/stageLLM)
	processingMu sync.Mutex
	thumbDir     string // cache dir for hi-res thumbnails
}

func (app *App) isDemo() bool {
	return app.client == nil
}

func (app *App) hiresThumbPath(ulid string) string {
	return filepath.Join(app.thumbDir, ulid+".png")
}

func (app *App) hiresThumbExists(ulid string) bool {
	_, err := os.Stat(app.hiresThumbPath(ulid))
	return err == nil
}

func generateHiresThumb(app *App, ulid, docType string) {
	if app.hiresThumbExists(ulid) {
		return
	}

	data, _, err := app.client.DownloadDocument(ulid)
	if err != nil {
		log.Printf("hires-thumb: download failed for %s: %v", ulid, err)
		return
	}

	tmpFile, err := os.CreateTemp("", "godocs-thumb-*"+docType)
	if err != nil {
		log.Printf("hires-thumb: temp file failed for %s: %v", ulid, err)
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		log.Printf("hires-thumb: write failed for %s: %v", ulid, err)
		return
	}
	tmpFile.Close()

	outPath := app.hiresThumbPath(ulid)
	if err := thumbnails.GenerateStyledAndSave(tmpPath, outPath, 600, thumbnails.StyleUniform); err != nil {
		log.Printf("hires-thumb: generation failed for %s: %v", ulid, err)
		return
	}
	log.Printf("hires-thumb: generated %s", ulid)
}

func processDocument(app *App, ulid, docType string) {
	defer func() {
		app.processingMu.Lock()
		delete(app.docStage, ulid)
		app.processingMu.Unlock()
	}()

	log.Printf("OCR: starting for %s (type=%s)", ulid, docType)

	// Download document
	data, _, err := app.client.DownloadDocument(ulid)
	if err != nil {
		log.Printf("OCR: download failed for %s: %v", ulid, err)
		return
	}

	// Write to temp file
	tmpFile, err := os.CreateTemp("", "godocs-ocr-*"+docType)
	if err != nil {
		log.Printf("OCR: temp file failed for %s: %v", ulid, err)
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		log.Printf("OCR: write failed for %s: %v", ulid, err)
		return
	}
	tmpFile.Close()

	// Run OCR
	text, err := ocr.ExtractText(tmpPath, docType)
	if err != nil {
		log.Printf("OCR: extraction failed for %s: %v", ulid, err)
		return
	}
	if text == "" {
		log.Printf("OCR: no text extracted for %s", ulid)
		return
	}
	log.Printf("OCR: extracted %d chars for %s", len(text), ulid)

	// Upload text back to godocs
	if err := app.client.UploadDocumentText(ulid, text); err != nil {
		log.Printf("OCR: upload text failed for %s: %v", ulid, err)
		return
	}

	// Transition to LLM stage
	app.processingMu.Lock()
	app.docStage[ulid] = stageLLM
	app.processingMu.Unlock()

	// Infer date via LLM
	ollamaURL := app.config.OllamaURL
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	model := app.config.OllamaModel
	if model == "" {
		model = "gemma3:4b"
	}

	dateStr, err := llm.InferDate(ollamaURL, model, text)
	if err != nil {
		log.Printf("OCR: date inference failed for %s: %v", ulid, err)
		return
	}
	if dateStr == "" {
		log.Printf("OCR: no date inferred for %s", ulid)
		return
	}

	log.Printf("OCR: inferred date %s for %s", dateStr, ulid)
	if err := app.client.UpdateDocumentDate(ulid, dateStr); err != nil {
		log.Printf("OCR: update date failed for %s: %v", ulid, err)
	} else {
		app.mu.Lock()
		app.llmDates[ulid] = true
		app.mu.Unlock()
	}
}

func (app *App) captureTagSet(ulid string) {
	if app.client == nil {
		return
	}
	tags, err := app.client.FetchDocTags(ulid)
	if err != nil || len(tags) == 0 {
		return
	}
	var entries []TagSetEntry
	var names []string
	for _, t := range tags {
		entries = append(entries, TagSetEntry{ID: t.ID, Name: t.Name, Color: t.Color})
		names = append(names, t.Name)
	}
	sort.Strings(names)
	label := strings.Join(names, ", ")
	newSet := RecentTagSet{Tags: entries, Label: label}

	// Dedup against existing sets
	var filtered []RecentTagSet
	for _, s := range app.recentSets {
		if s.Label != newSet.Label {
			filtered = append(filtered, s)
		}
	}
	app.recentSets = append([]RecentTagSet{newSet}, filtered...)
	if len(app.recentSets) > 3 {
		app.recentSets = app.recentSets[:3]
	}
}

func (app *App) buildTagGroups(ulid string) ([]EditTagGroup, []string) {
	activeTags := make(map[int]bool)
	if docTags, err := app.client.FetchDocTags(ulid); err == nil {
		for _, t := range docTags {
			activeTags[t.ID] = true
		}
	}

	groupMap := make(map[string][]EditTagItem)
	var groupOrder []string
	var allTags []GodocsTag
	for _, t := range app.client.tags {
		allTags = append(allTags, t)
	}
	sort.Slice(allTags, func(i, j int) bool {
		if allTags[i].TagGroup != allTags[j].TagGroup {
			return allTags[i].TagGroup < allTags[j].TagGroup
		}
		if allTags[i].SortOrder != allTags[j].SortOrder {
			return allTags[i].SortOrder < allTags[j].SortOrder
		}
		return allTags[i].Name < allTags[j].Name
	})
	for _, t := range allTags {
		group := t.TagGroup
		if group == "" {
			group = "Other"
		}
		if _, exists := groupMap[group]; !exists {
			groupOrder = append(groupOrder, group)
		}
		groupMap[group] = append(groupMap[group], EditTagItem{
			ID:     t.ID,
			Name:   t.Name,
			Color:  t.Color,
			Group:  group,
			Active: activeTags[t.ID],
		})
	}

	var groups []EditTagGroup
	for _, g := range groupOrder {
		groups = append(groups, EditTagGroup{Name: g, Tags: groupMap[g]})
	}

	tagGroups, _ := app.client.FetchTagGroups()
	return groups, tagGroups
}

// --- Page data ---

type InboxItem struct {
	// Server mode
	ULID          string
	Name          string
	DocType       string
	Folder        string
	IngressTime   string
	ThumbnailURL  string // full URL
	ViewURL       string // full URL
	TextPreview   string
	HasThumbnail  bool
	HasHiresThumb bool
	Processing    bool
	LLMWorking    bool
	DocumentDate  string
	DateIsLLM     bool
	// Demo mode
	Content template.HTML
}

type PageData struct {
	Page       string
	Item       *InboxItem
	Shortcuts  []ShortcutConfig
	Remaining  int
	Done       bool
	Undoable   bool
	UndoInfo   string
	Flash      string
	IsDemo     bool
	GodocsURL  string
	Groups     []EditTagGroup
	TagGroups  []string
	RecentSets []RecentTagSet
}

type TaggedGroup struct {
	Name  string
	Items []string
}

type TaggedPageData struct {
	Page   string
	Groups []TaggedGroup
	Total  int
	IsDemo bool
}

type EditTagItem struct {
	ID     int
	Name   string
	Color  string
	Group  string
	Active bool
}

type EditTagGroup struct {
	Name string
	Tags []EditTagItem
}

type AboutPageData struct {
	Page         string
	Config       Config
	ConfigSource string
	ServerTags   []GodocsTag
	IsDemo       bool
	GodocsURL    string
}

// --- Demo defaults ---

var defaultDemoTags = []ShortcutConfig{
	{Key: "r", Name: "reference"},
	{Key: "a", Name: "action"},
	{Key: "t", Name: "trash"},
	{Key: "p", Name: "project"},
	{Key: "s", Name: "someday"},
	{Key: "w", Name: "waiting"},
}

var demoFiles = map[string]string{
	"meeting-notes-2024-q4.md":       "# Q4 Planning Meeting Notes\n\nAttendees: Alice, Bob, Charlie\n\n## Action Items\n- Review budget proposal by Friday\n- Schedule follow-up with vendor\n",
	"api-design-v2.md":               "# API v2 Design Notes\n\n## Breaking Changes\n- Auth moves to Bearer tokens\n- Cursor-based pagination\n",
	"old-server-config.txt":          "Server: web-prod-03\nStatus: DECOMMISSIONED 2024-01-15\nCan be deleted after 2025-01-15.\n",
	"research-llm-classification.md": "# LLM-based Document Classification\n\nUse a local LLM to auto-suggest tags for inbox items.\n",
	"todo-fix-backup-script.md":      "# Fix Backup Script\n\nError: \"permission denied: /mnt/backup-v2/daily\"\n",
}

// --- Config ---

func defaultConfig() Config {
	return Config{
		Addr: ":8080",
	}
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

func writeExampleConfig(path string) error {
	cfg := Config{
		GodocsServer: "http://test:8000",
		Addr:         ":8080",
		Shortcuts: []ShortcutConfig{
			{Key: "l", TagID: 18},
			{Key: "m", TagID: 20},
			{Key: "h", TagID: 13},
			{Key: "c", TagID: 10},
		},
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	header := "# godocs-inbox configuration\n# Tag IDs come from your godocs server: GET /api/tags\n\n"
	return os.WriteFile(path, []byte(header+string(data)), 0644)
}

func seedDemo(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for name, content := range demoFiles {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			return err
		}
	}
	return nil
}

// --- Main ---

func main() {
	demo := flag.Bool("demo", false, "Run with sample demo data (no godocs server needed)")
	initCfg := flag.Bool("init", false, "Write an example "+configFileName+" and exit")
	addr := flag.String("addr", "", "Override listen address (e.g. :9090)")
	flag.Usage = printUsage
	flag.Parse()

	if *initCfg {
		if err := writeExampleConfig(configFileName); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Wrote %s\n", configFileName)
		return
	}

	var app *App

	switch {
	case *demo:
		cfg := defaultConfig()
		cfg.InboxDir = "./demo-inbox"
		cfg.TaggedDir = "./demo-tagged"
		cfg.Shortcuts = defaultDemoTags
		if err := seedDemo(cfg.InboxDir); err != nil {
			fmt.Fprintf(os.Stderr, "Error seeding demo: %v\n", err)
			os.Exit(1)
		}
		os.MkdirAll(cfg.TaggedDir, 0755)
		app = &App{config: cfg, configFile: "demo", llmDates: make(map[string]bool), docStage: make(map[string]string)}
		log.Println("Running in demo mode (local files, no godocs server)")

	default:
		cfg, err := loadConfig(configFileName)
		if err != nil {
			if os.IsNotExist(err) {
				printUsage()
				os.Exit(0)
			}
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if cfg.GodocsServer == "" {
			fmt.Fprintf(os.Stderr, "Error: godocs_server must be set in %s\n", configFileName)
			os.Exit(1)
		}
		if len(cfg.Shortcuts) == 0 {
			fmt.Fprintf(os.Stderr, "Error: at least one tag shortcut must be configured in %s\n", configFileName)
			os.Exit(1)
		}

		// Connect to godocs and validate tags
		client := NewGodocsClient(cfg.GodocsServer)
		serverTags, err := client.FetchTags()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error connecting to godocs at %s: %v\n", cfg.GodocsServer, err)
			os.Exit(1)
		}
		log.Printf("Connected to godocs at %s (%d tags available)", cfg.GodocsServer, len(serverTags))

		// Populate shortcut names from server
		for i := range cfg.Shortcuts {
			t, ok := client.tags[cfg.Shortcuts[i].TagID]
			if !ok {
				fmt.Fprintf(os.Stderr, "Error: tag_id %d (key '%s') not found on server\n",
					cfg.Shortcuts[i].TagID, cfg.Shortcuts[i].Key)
				fmt.Fprintf(os.Stderr, "Available tags:\n")
				for _, st := range serverTags {
					fmt.Fprintf(os.Stderr, "  id=%d  name=%s  group=%s\n", st.ID, st.Name, st.TagGroup)
				}
				os.Exit(1)
			}
			cfg.Shortcuts[i].Name = t.Name
			cfg.Shortcuts[i].Color = t.Color
		}

		// Check for reserved key collisions
		reservedKeys := map[string]string{
			"1": "recent tag set 1", "2": "recent tag set 2", "3": "recent tag set 3",
			"d": "done/next", "u": "undo",
		}
		for _, s := range cfg.Shortcuts {
			if desc, ok := reservedKeys[s.Key]; ok {
				log.Printf("WARNING: shortcut key '%s' (%s) collides with reserved key for %s", s.Key, s.Name, desc)
			}
		}

		absPath, _ := filepath.Abs(configFileName)
		cacheDir, _ := os.UserCacheDir()
		thumbDir := filepath.Join(cacheDir, "godocs-inbox", "thumbs")
		os.MkdirAll(thumbDir, 0755)
		app = &App{config: cfg, configFile: absPath, client: client, llmDates: make(map[string]bool), docStage: make(map[string]string), thumbDir: thumbDir}
	}

	if *addr != "" {
		app.config.Addr = *addr
	}

	serve(app)
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `godocs-inbox - keyboard-driven document triage for godocs

Usage:
  godocs-inbox              Run using %s (connects to godocs server)
  godocs-inbox -demo        Run with built-in sample data (no server needed)
  godocs-inbox -init        Create an example %s
  godocs-inbox -addr :9090  Override listen address

If no flags are given and no %s is found, this help is shown.

Config file fields:
  godocs_server   URL of the godocs server (e.g. http://test:8000)
  addr            Listen address (default: :8080)
  tags            List of {key, tag_id} shortcut definitions
                  Tag IDs come from your godocs server: GET /api/tags

`, configFileName, configFileName, configFileName)
	flag.PrintDefaults()
}

// --- Server ---

func serve(app *App) {
	funcMap := template.FuncMap{
		"add": func(a, b int) int { return a + b },
	}
	tmpl := template.Must(template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html"))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		app.mu.Lock()
		defer app.mu.Unlock()

		flash := r.URL.Query().Get("flash")
		data := PageData{
			Page:      "inbox",
			Shortcuts: app.config.Shortcuts,
			Flash:     flash,
			Undoable:  app.lastAction != nil,
			IsDemo:    app.isDemo(),
			GodocsURL: app.config.GodocsServer,
		}
		if app.lastAction != nil {
			data.UndoInfo = app.lastAction.DocName
			if data.UndoInfo == "" {
				data.UndoInfo = app.lastAction.File
			}
		}

		if app.isDemo() {
			items := listFiles(app.config.InboxDir)
			data.Remaining = len(items)
			if len(items) == 0 {
				data.Done = true
			} else {
				content, _ := os.ReadFile(filepath.Join(app.config.InboxDir, items[0]))
				data.Item = &InboxItem{
					Name:    items[0],
					Content: template.HTML("<pre>" + template.HTMLEscapeString(string(content)) + "</pre>"),
				}
			}
		} else {
			sr, err := app.client.FetchUntagged(1, 1)
			if err != nil {
				log.Printf("error fetching untagged: %v", err)
				http.Error(w, "Error connecting to godocs server", 502)
				return
			}
			data.Remaining = sr.TotalCount
			if sr.TotalCount == 0 {
				data.Done = true
			} else {
				doc := sr.Documents[0]
				item := &InboxItem{
					ULID:    doc.ULID,
					Name:    doc.Name,
					DocType: doc.DocumentType,
					Folder:  doc.Folder,
				}
				// Fetch status for thumbnail/text info
				if status, err := app.client.FetchDocStatus(doc.ULID); err == nil {
					item.HasThumbnail = status.HasThumbnail
					if status.HasThumbnail {
						item.ThumbnailURL = app.config.GodocsServer + status.ThumbnailURL
					}
					item.ViewURL = app.config.GodocsServer + status.ViewURL
					item.IngressTime = status.IngressTime
					item.DocumentDate = status.DocumentDate
					item.DateIsLLM = app.llmDates[doc.ULID]

					// Check background processing stage
					app.processingMu.Lock()
					stage := app.docStage[doc.ULID]
					app.processingMu.Unlock()

					switch stage {
					case stageOCR:
						item.Processing = true
					case stageLLM:
						item.LLMWorking = true
					}

					// Trigger OCR if no text and not already in pipeline
					if !status.HasText && stage == "" {
						app.processingMu.Lock()
						if app.docStage[doc.ULID] == "" {
							app.docStage[doc.ULID] = stageOCR
							go processDocument(app, doc.ULID, status.DocumentType)
						}
						app.processingMu.Unlock()
						item.Processing = true
					}

					// Hi-res thumbnail: check cache, trigger generation
					if status.HasThumbnail {
						if app.hiresThumbExists(doc.ULID) {
							item.HasHiresThumb = true
						} else {
							go generateHiresThumb(app, doc.ULID, status.DocumentType)
						}
					}
				}
				// Fetch text preview
				if text, err := app.client.FetchDocText(doc.ULID); err == nil && text != "" {
					if len(text) > 2000 {
						text = text[:2000] + "..."
					}
					item.TextPreview = text
				}
				data.Item = item
				data.Groups, data.TagGroups = app.buildTagGroups(doc.ULID)
				data.RecentSets = app.recentSets
			}
		}

		tmpl.ExecuteTemplate(w, "index.html", data)
	})

	http.HandleFunc("/tag", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		app.mu.Lock()
		defer app.mu.Unlock()

		tagKey := r.FormValue("tag")

		if app.isDemo() {
			item := r.FormValue("item")
			tagName := ""
			for _, s := range app.config.Shortcuts {
				if s.Key == tagKey {
					tagName = s.Name
					break
				}
			}
			if tagName == "" || item == "" {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			destDir := filepath.Join(app.config.TaggedDir, tagName)
			os.MkdirAll(destDir, 0755)
			src := filepath.Join(app.config.InboxDir, item)
			dst := filepath.Join(destDir, item)
			if err := os.Rename(src, dst); err != nil {
				log.Printf("error moving %s: %v", item, err)
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			app.lastAction = &LastAction{File: item, FromDir: app.config.InboxDir, ToDir: destDir}
			flash := tagKey + ":" + tagName + " \u2190 " + item
			http.Redirect(w, r, "/?flash="+flash, http.StatusSeeOther)
		} else {
			docULID := r.FormValue("ulid")
			docName := r.FormValue("name")
			var shortcut *ShortcutConfig
			for i := range app.config.Shortcuts {
				if app.config.Shortcuts[i].Key == tagKey {
					shortcut = &app.config.Shortcuts[i]
					break
				}
			}
			if shortcut == nil || docULID == "" {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			if err := app.client.AddTag(docULID, shortcut.TagID); err != nil {
				log.Printf("error tagging %s with %s: %v", docULID, shortcut.Name, err)
				http.Redirect(w, r, "/?flash=Error: "+err.Error(), http.StatusSeeOther)
				return
			}
			app.captureTagSet(docULID)
			app.lastAction = &LastAction{
				DocULID: docULID,
				DocName: docName,
				TagID:   shortcut.TagID,
				TagName: shortcut.Name,
			}
			flash := shortcut.Key + ":" + shortcut.Name + " \u2190 " + docName
			http.Redirect(w, r, "/?flash="+flash, http.StatusSeeOther)
		}
	})

	http.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		app.mu.Lock()
		defer app.mu.Unlock()

		if !app.isDemo() {
			ulid := r.FormValue("ulid")
			if ulid != "" {
				app.captureTagSet(ulid)
			}
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	http.HandleFunc("/api/apply-tagset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || app.isDemo() {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		app.mu.Lock()
		defer app.mu.Unlock()

		ulid := r.FormValue("ulid")
		docName := r.FormValue("name")
		indexStr := r.FormValue("index")
		index, err := strconv.Atoi(indexStr)
		if err != nil || index < 0 || index >= len(app.recentSets) || ulid == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		set := app.recentSets[index]
		for _, tag := range set.Tags {
			if err := app.client.AddTag(ulid, tag.ID); err != nil {
				log.Printf("apply-tagset: error adding tag %d to %s: %v", tag.ID, ulid, err)
			}
		}
		app.captureTagSet(ulid)
		flash := set.Label + " ← " + docName
		http.Redirect(w, r, "/?flash="+flash, http.StatusSeeOther)
	})

	http.HandleFunc("/undo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		app.mu.Lock()
		defer app.mu.Unlock()

		if app.lastAction == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		if app.isDemo() {
			src := filepath.Join(app.lastAction.ToDir, app.lastAction.File)
			dst := filepath.Join(app.lastAction.FromDir, app.lastAction.File)
			if err := os.Rename(src, dst); err != nil {
				log.Printf("error undoing %s: %v", app.lastAction.File, err)
			}
			flash := "undo \u2190 " + app.lastAction.File
			app.lastAction = nil
			http.Redirect(w, r, "/?flash="+flash, http.StatusSeeOther)
		} else {
			if err := app.client.RemoveTag(app.lastAction.DocULID, app.lastAction.TagID); err != nil {
				log.Printf("error undoing tag on %s: %v", app.lastAction.DocULID, err)
			}
			flash := "undo \u2190 " + app.lastAction.DocName
			app.lastAction = nil
			http.Redirect(w, r, "/?flash="+flash, http.StatusSeeOther)
		}
	})

	http.HandleFunc("/tagged", func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		defer app.mu.Unlock()

		data := TaggedPageData{Page: "tagged", IsDemo: app.isDemo()}

		if app.isDemo() {
			for _, s := range app.config.Shortcuts {
				dir := filepath.Join(app.config.TaggedDir, s.Name)
				items := listFiles(dir)
				if len(items) > 0 {
					data.Groups = append(data.Groups, TaggedGroup{Name: s.Name, Items: items})
					data.Total += len(items)
				}
			}
		}
		// In server mode, tagged view is not applicable (use godocs UI)

		tmpl.ExecuteTemplate(w, "tagged.html", data)
	})

	http.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		defer app.mu.Unlock()

		data := AboutPageData{
			Page:         "about",
			Config:       app.config,
			ConfigSource: app.configFile,
			IsDemo:       app.isDemo(),
			GodocsURL:    app.config.GodocsServer,
		}
		if app.client != nil {
			for _, t := range app.client.tags {
				data.ServerTags = append(data.ServerTags, t)
			}
			sort.Slice(data.ServerTags, func(i, j int) bool {
				return data.ServerTags[i].Name < data.ServerTags[j].Name
			})
		}

		tmpl.ExecuteTemplate(w, "about.html", data)
	})

	http.HandleFunc("/api/toggle-tag", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || app.isDemo() {
			http.Error(w, "not allowed", 405)
			return
		}
		app.mu.Lock()
		defer app.mu.Unlock()

		var req struct {
			ULID   string `json:"ulid"`
			TagID  int    `json:"tag_id"`
			Active bool   `json:"active"` // current state: true=remove, false=add
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}

		var err error
		if req.Active {
			err = app.client.RemoveTag(req.ULID, req.TagID)
		} else {
			err = app.client.AddTag(req.ULID, req.TagID)
		}

		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			log.Printf("toggle-tag error: %v", err)
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"active": !req.Active})
	})

	http.HandleFunc("/api/create-tag", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || app.isDemo() {
			http.Error(w, "not allowed", 405)
			return
		}
		app.mu.Lock()
		defer app.mu.Unlock()

		var req struct {
			Name  string `json:"name"`
			Color string `json:"color"`
			Group string `json:"group"`
			ULID  string `json:"ulid"` // if set, also apply to this document
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "name is required"})
			return
		}
		if req.Color == "" {
			req.Color = "#3498db"
		}

		tag, err := app.client.CreateTag(req.Name, req.Color, req.Group)
		if err != nil {
			log.Printf("create-tag error: %v", err)
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		// Auto-apply to document if ulid provided
		applied := false
		if req.ULID != "" {
			if err := app.client.AddTag(req.ULID, tag.ID); err != nil {
				log.Printf("auto-apply tag %d to %s failed: %v", tag.ID, req.ULID, err)
			} else {
				applied = true
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      tag.ID,
			"name":    tag.Name,
			"color":   tag.Color,
			"group":   tag.TagGroup,
			"applied": applied,
		})
	})

	// Proxy thumbnail requests to avoid CORS issues
	http.HandleFunc("/proxy/thumbnail/", func(w http.ResponseWriter, r *http.Request) {
		if app.isDemo() {
			http.NotFound(w, r)
			return
		}
		ulid := strings.TrimPrefix(r.URL.Path, "/proxy/thumbnail/")
		resp, err := app.client.httpClient.Get(app.config.GodocsServer + "/api/document/" + ulid + "/thumbnail")
		if err != nil {
			http.Error(w, "upstream error", 502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.Header().Set("Cache-Control", "public, max-age=3600")
		io.Copy(w, resp.Body)
	})

	// Serve cached hi-res thumbnails
	http.HandleFunc("/hires/thumbnail/", func(w http.ResponseWriter, r *http.Request) {
		if app.isDemo() {
			http.NotFound(w, r)
			return
		}
		ulid := strings.TrimPrefix(r.URL.Path, "/hires/thumbnail/")
		path := app.hiresThumbPath(ulid)
		if _, err := os.Stat(path); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, path)
	})

	// Check if hi-res thumbnail is ready (for JS polling)
	http.HandleFunc("/hires/thumbnail-ready/", func(w http.ResponseWriter, r *http.Request) {
		if app.isDemo() {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"ready": false})
			return
		}
		ulid := strings.TrimPrefix(r.URL.Path, "/hires/thumbnail-ready/")
		ready := app.hiresThumbExists(ulid)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ready": ready})
	})

	log.Printf("godocs-inbox serving on http://localhost%s", app.config.Addr)
	if !app.isDemo() {
		log.Printf("  godocs server: %s", app.config.GodocsServer)
		log.Printf("  shortcuts: %d configured", len(app.config.Shortcuts))
	}
	log.Fatal(http.ListenAndServe(app.config.Addr, nil))
}

func listFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var items []string
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			items = append(items, e.Name())
		}
	}
	sort.Strings(items)
	return items
}
