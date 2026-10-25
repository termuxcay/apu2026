package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	baseURL     = "https://resultados.tse.jus.br/oficial"
	cycle       = "ele2026"
	electionID  = 6258
	pollTicker  = 30 * time.Second
	httpTimeout = 12 * time.Second
	ufWorkers   = 8
)

type ufEntry struct {
	Code       string            `json:"code"`
	Sections   int               `json:"sections"`
	SectionsTS int               `json:"sectionsTotal"`
	Pct        string            `json:"pct"`
	Votes      map[string]int    `json:"votes"`
	Shares     map[string]string `json:"shares"`
}

type cand struct {
	Name    string `json:"name"`
	Number  string `json:"number"`
	Party   string `json:"party"`
	Votes   int    `json:"votes"`
	Share   string `json:"share"`
	Elected bool   `json:"elected"`
	SqCand  string `json:"sqcand"`
}

type snapshot struct {
	GeneratedID string
	GeneratedAt string
	TotalizedAt string
	Progress    string
	Sections    int
	SectionsTS  int
	Electorate  int
	Turnout     int
	Valid       int
	Null        int
	Blank       int
	Final       bool
	Winner      string
	Candidates  []cand
	LastSync    time.Time
}

type tseCandidate struct {
	N      string `json:"n"`
	Nmu    string `json:"nmu"`
	SqCand string `json:"sqcand"`
	E      string `json:"e"`
	Vap    string `json:"vap"`
	Pvap   string `json:"pvap"`
}

type tseParty struct {
	N    string         `json:"n"`
	Sg   string         `json:"sg"`
	Cand []tseCandidate `json:"cand"`
}

type tseAggregate struct {
	N   string     `json:"n"`
	Tp  string     `json:"tp"`
	Par []tseParty `json:"par"`
}

type tseCargo struct {
	Cd  string         `json:"cd"`
	Agr []tseAggregate `json:"agr"`
}

type tseSections struct {
	TS  string `json:"ts"`
	ST  string `json:"st"`
	PST string `json:"pst"`
}

type tseElectorate struct {
	TE string `json:"te"`
	C  string `json:"c"`
}

type tseVotes struct {
	VV string `json:"vv"`
	VN string `json:"vn"`
	VB string `json:"vb"`
}

type tseFile struct {
	Ele  string        `json:"ele"`
	T    string        `json:"t"`
	Dg   string        `json:"dg"`
	Hg   string        `json:"hg"`
	IDG  string        `json:"idg"`
	Dt   string        `json:"dt"`
	Ht   string        `json:"ht"`
	Dv   string        `json:"dv"`
	Tf   string        `json:"tf"`
	Md   string        `json:"md"`
	And  string        `json:"and"`
	Carg []tseCargo    `json:"carg"`
	S    tseSections   `json:"s"`
	E    tseElectorate `json:"e"`
	V    tseVotes      `json:"v"`
}

type tseAbr struct {
	TpAbr string      `json:"tpabr"`
	CdAbr string      `json:"cdabr"`
	And   string      `json:"and"`
	S     tseSections `json:"s"`
}

type tseProgress struct {
	IDG string   `json:"idg"`
	Abr []tseAbr `json:"abr"`
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

var ufs = []string{"ac", "al", "am", "ap", "ba", "ce", "df", "es", "go", "ma", "mg", "ms", "mt", "pa", "pb", "pe", "pi", "pr", "rj", "rn", "ro", "rr", "rs", "sc", "se", "sp", "to"}

var httpClient = &http.Client{Timeout: httpTimeout}

var etagCache = struct {
	sync.RWMutex
	m map[string]string
}{m: map[string]string{}}

func fetchJSON(url string, target any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "apuracao2026-live/2.0")
	req.Header.Set("Accept", "application/json")

	etagCache.RLock()
	etag, ok := etagCache.m[url]
	etagCache.RUnlock()
	if ok {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil
	case http.StatusOK:
	default:
		return fmt.Errorf("tse returned %d for %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	if et := resp.Header.Get("ETag"); et != "" {
		etagCache.Lock()
		etagCache.m[url] = et
		etagCache.Unlock()
	}
	return nil
}

func parseCandidates(f *tseFile) []cand {
	seen := map[string]bool{}
	out := []cand{}
	for _, cargo := range f.Carg {
		if cargo.Cd != "1" {
			continue
		}
		for _, agr := range cargo.Agr {
			for _, party := range agr.Par {
				for _, c := range party.Cand {
					if seen[c.N] {
						continue
					}
					seen[c.N] = true
					out = append(out, cand{
						Name:    c.Nmu,
						Number:  c.N,
						Party:   party.Sg,
						Votes:   atoi(c.Vap),
						Share:   c.Pvap,
						Elected: c.E == "s",
						SqCand:  c.SqCand,
					})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Votes > out[j].Votes })
	return out
}

type store struct {
	mu        sync.RWMutex
	snap      snapshot
	lastError string
	ready     bool
}

var resultsStore = &store{}

func applyResults(f *tseFile) {
	cands := parseCandidates(f)
	if len(cands) < 2 {
		return
	}
	winner := ""
	for _, c := range cands {
		if c.Elected {
			winner = c.Number
			break
		}
	}
	final := f.Tf == "s" || winner != "" || f.Md == "e"
	s := snapshot{
		GeneratedID: f.IDG,
		GeneratedAt: f.Dg + " " + f.Hg,
		TotalizedAt: strings.TrimSpace(f.Dt + " " + f.Ht),
		Progress:    f.S.PST,
		Sections:    atoi(f.S.ST),
		SectionsTS:  atoi(f.S.TS),
		Electorate:  atoi(f.E.TE),
		Turnout:     atoi(f.E.C),
		Valid:       atoi(f.V.VV),
		Null:        atoi(f.V.VN),
		Blank:       atoi(f.V.VB),
		Final:       final,
		Winner:      winner,
		Candidates:  cands,
		LastSync:    time.Now(),
	}
	resultsStore.mu.Lock()
	resultsStore.snap = s
	resultsStore.ready = true
	resultsStore.mu.Unlock()
}

var ufStore = struct {
	sync.RWMutex
	entries map[string]ufEntry
}{entries: map[string]ufEntry{}}

func fetchUF(uf string) {
	url := fmt.Sprintf("%s/%s/%d/dados/%s/%s-c0001-e%06d-u.json", baseURL, cycle, electionID, uf, uf, electionID)
	var f tseFile
	if err := fetchJSON(url, &f); err != nil {
		return
	}
	entry := ufEntry{
		Code:       strings.ToUpper(uf),
		Sections:   atoi(f.S.ST),
		SectionsTS: atoi(f.S.TS),
		Pct:        f.S.PST,
		Votes:      map[string]int{},
		Shares:     map[string]string{},
	}
	for _, c := range parseCandidates(&f) {
		entry.Votes[c.Number] = c.Votes
		entry.Shares[c.Number] = c.Share
	}
	ufStore.Lock()
	ufStore.entries[entry.Code] = entry
	ufStore.Unlock()
}

func fetchAllUFs() {
	sem := make(chan struct{}, ufWorkers)
	var wg sync.WaitGroup
	for _, uf := range ufs {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fetchUF(u)
		}(uf)
	}
	wg.Wait()
}

func poller() {
	resultsURL := fmt.Sprintf("%s/%s/%d/dados/br/br-c0001-e%06d-u.json", baseURL, cycle, electionID, electionID)
	progressURL := fmt.Sprintf("%s/%s/%d/dados/br/br-e%06d-ab.json", baseURL, cycle, electionID, electionID)

	run := func() {
		var f tseFile
		if err := fetchJSON(resultsURL, &f); err != nil {
			resultsStore.mu.Lock()
			resultsStore.lastError = err.Error()
			resultsStore.mu.Unlock()
		} else {
			resultsStore.mu.Lock()
			resultsStore.lastError = ""
			resultsStore.mu.Unlock()
			applyResults(&f)
		}
		var p tseProgress
		if err := fetchJSON(progressURL, &p); err == nil {
			for _, abr := range p.Abr {
				if abr.TpAbr != "uf" {
					continue
				}
				code := strings.ToUpper(abr.CdAbr)
				ufStore.Lock()
				if entry, exists := ufStore.entries[code]; exists {
					entry.Sections = atoi(abr.S.ST)
					entry.SectionsTS = atoi(abr.S.TS)
					entry.Pct = abr.S.PST
					ufStore.entries[code] = entry
				}
				ufStore.Unlock()
			}
		}
		fetchAllUFs()
	}

	run()
	for {
		time.Sleep(pollTicker)
		run()
	}
}

type apiSnapshot struct {
	OK          bool      `json:"ok"`
	IDG         string    `json:"idg"`
	GeneratedAt string    `json:"generatedAt"`
	TotalizedAt string    `json:"totalizedAt"`
	Progress    string    `json:"progress"`
	Sections    int       `json:"sections"`
	SectionsTS  int       `json:"sectionsTotal"`
	Electorate  int       `json:"electorate"`
	Turnout     int       `json:"turnout"`
	Valid       int       `json:"validVotes"`
	Null        int       `json:"nullVotes"`
	Blank       int       `json:"blankVotes"`
	Final       bool      `json:"final"`
	Winner      string    `json:"winnerNumber"`
	Candidates  []cand    `json:"candidates"`
	UFs         []ufEntry `json:"ufs"`
	LastSync    time.Time `json:"lastSync"`
	Source      string    `json:"source"`
	Error       string    `json:"error,omitempty"`
}

func handleResults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	resultsStore.mu.RLock()
	s := resultsStore.snap
	ready := resultsStore.ready
	lastErr := resultsStore.lastError
	resultsStore.mu.RUnlock()

	out := apiSnapshot{OK: ready, Source: "TSE"}
	if ready {
		out.IDG = s.GeneratedID
		out.GeneratedAt = s.GeneratedAt
		out.TotalizedAt = s.TotalizedAt
		out.Progress = s.Progress
		out.Sections = s.Sections
		out.SectionsTS = s.SectionsTS
		out.Electorate = s.Electorate
		out.Turnout = s.Turnout
		out.Valid = s.Valid
		out.Null = s.Null
		out.Blank = s.Blank
		out.Final = s.Final
		out.Winner = s.Winner
		out.Candidates = s.Candidates
		out.LastSync = s.LastSync
	}

	ufStore.RLock()
	for _, entry := range ufStore.entries {
		out.UFs = append(out.UFs, entry)
	}
	ufStore.RUnlock()
	sort.Slice(out.UFs, func(i, j int) bool { return out.UFs[i].Code < out.UFs[j].Code })

	if out.UFs == nil {
		out.UFs = []ufEntry{}
	}
	if out.Candidates == nil {
		out.Candidates = []cand{}
	}
	if !ready && lastErr != "" {
		out.Error = lastErr
	}
	json.NewEncoder(w).Encode(out)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resultsStore.mu.RLock()
	ready := resultsStore.ready
	errMsg := resultsStore.lastError
	resultsStore.mu.RUnlock()
	ufStore.RLock()
	ufCount := len(ufStore.entries)
	ufStore.RUnlock()
	json.NewEncoder(w).Encode(map[string]any{"ready": ready, "error": errMsg, "ufFiles": ufCount, "time": time.Now().Format(time.RFC3339)})
}

type newsItem struct {
	Title  string `json:"title"`
	Source string `json:"source"`
	Date   string `json:"date"`
	Link   string `json:"link"`
}

var newsCache = struct {
	sync.RWMutex
	items   []newsItem
	fetched time.Time
}{}

var (
	newsItemRe   = regexp.MustCompile(`(?s)<item>(.*?)</item>`)
	newsTitleRe  = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
	newsDateRe   = regexp.MustCompile(`(?s)<pubDate>(.*?)</pubDate>`)
	newsSrcRe    = regexp.MustCompile(`(?s)<source[^>]*>(.*?)</source>`)
	newsLinkRe   = regexp.MustCompile(`(?s)<link>(.*?)</link>`)
	newsEntities = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&#39;", "'", "&apos;", "'", "<![CDATA[", "", "]]>", "")
)

func newsField(xml string, re *regexp.Regexp) string {
	if m := re.FindStringSubmatch(xml); m != nil {
		return strings.TrimSpace(newsEntities.Replace(m[1]))
	}
	return ""
}

func fetchNews() []newsItem {
	url := "https://news.google.com/rss/search?q=%22segundo+turno%22+presidenci%C3%A1veis+apura%C3%A7%C3%A3o&hl=pt-BR&gl=BR&ceid=BR:pt-419"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	out := []newsItem{}
	for _, m := range newsItemRe.FindAllStringSubmatch(string(body), 12) {
		out = append(out, newsItem{
			Title:  newsField(m[1], newsTitleRe),
			Date:   newsField(m[1], newsDateRe),
			Source: newsField(m[1], newsSrcRe),
			Link:   newsField(m[1], newsLinkRe),
		})
	}
	return out
}

func handleNews(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	newsCache.RLock()
	fresh := time.Since(newsCache.fetched) < 5*time.Minute
	items := newsCache.items
	newsCache.RUnlock()
	if !fresh {
		fetched := fetchNews()
		if len(fetched) > 0 {
			newsCache.Lock()
			newsCache.items = fetched
			newsCache.fetched = time.Now()
			newsCache.Unlock()
			items = fetched
		}
	}
	if items == nil {
		items = []newsItem{}
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": len(items) > 0, "items": items})
}

func main() {
	go poller()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/results", handleResults)
	mux.HandleFunc("/api/health", handleHealth)
	mux.HandleFunc("/api/news", handleNews)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "static/index.html")
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := "127.0.0.1:" + port
	log.Printf("apuração 2026 · 2º turno · listening on http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
