// IRC manager: a single long-lived girc client that talks to the
// irchighway.net #ebooks search/download bots, with a 10-second
// rate limit on search requests and FIFO correlation of incoming
// DCC SEND files back to pending requests.
package ircclient

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lrstanley/girc"
)

const (
	DefaultServer     = "irc.irchighway.net"
	DefaultPort       = 6697
	DefaultChannel    = "#ebooks"
	DefaultSearchBot  = "search"
	DefaultUserAgent  = "OpenBooks 4.3.0" // irchighway-allowlisted; bump when upstream openbooks does
	DefaultSearchGap  = 10 * time.Second
	connectAfterDelay = 2 * time.Second
)

// Callbacks are invoked from the IRC reader goroutine. Implementations
// must be safe to call concurrently and should return quickly (do
// PocketBase work asynchronously if it could block).
type Callbacks struct {
	OnConnected      func(nick string)
	OnDisconnected   func()
	OnSearchAccepted func(searchID string)
	OnSearchMatches  func(searchID string, count int)
	OnSearchResults  func(searchID string, books []BookResult)
	OnSearchFailed   func(searchID, errMsg string)
	OnDownloadStart  func(downloadID, filename string, size int64)
	OnDownloadDone   func(downloadID, filePath, filename string, size int64)
	OnDownloadFailed func(downloadID, errMsg string)
}

type Config struct {
	Server     string
	Port       int
	Channel    string
	SearchBot  string
	UserAgent  string
	Nick       string
	SearchGap  time.Duration
	WorkDir    string // where DCC files are written
	Logger     *log.Logger
	Callbacks  Callbacks
}

func (c *Config) applyDefaults() {
	if c.Server == "" {
		c.Server = DefaultServer
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.Channel == "" {
		c.Channel = DefaultChannel
	}
	if c.SearchBot == "" {
		c.SearchBot = DefaultSearchBot
	}
	if c.UserAgent == "" {
		c.UserAgent = DefaultUserAgent
	}
	if c.SearchGap == 0 {
		c.SearchGap = DefaultSearchGap
	}
	if c.Nick == "" {
		c.Nick = RandomNick()
	}
	if c.Logger == nil {
		c.Logger = log.Default()
	}
}

// RandomNick returns "pb_" + 8 hex chars — matches openbooks' default
// shape and is short enough for IRC nick length limits.
func RandomNick() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "pb_" + hex.EncodeToString(b[:])
}

type pendingSearch struct {
	id    string
	query string
}

type pendingDownload struct {
	id          string
	command     string
	filenameHint string // best-effort filename inside the command
}

type Manager struct {
	cfg       Config
	client    *girc.Client
	searchCh  chan pendingSearch

	mu            sync.Mutex
	connected     bool
	nick          string
	lastSearchAt  time.Time
	pendingSearch []pendingSearch    // FIFO of accepted searches waiting on results
	pendingDLs    []pendingDownload  // FIFO of accepted downloads waiting on DCC SEND
}

func New(cfg Config) *Manager {
	cfg.applyDefaults()
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(os.TempDir(), "ebooks_dcc")
	}
	_ = os.MkdirAll(cfg.WorkDir, 0o755)

	m := &Manager{
		cfg:      cfg,
		searchCh: make(chan pendingSearch, 256),
	}
	m.client = girc.New(girc.Config{
		Server: cfg.Server,
		Port:   cfg.Port,
		Nick:   cfg.Nick,
		User:   cfg.Nick,
		Name:   cfg.Nick,
		SSL:    true,
		// irchighway.net's cert SAN doesn't match the hostname; openbooks
		// uses the same skip-verify and has done so for years without issue.
		TLSConfig:  &tls.Config{InsecureSkipVerify: true, ServerName: cfg.Server},
		AllowFlood: true, // we self-throttle searches; no need for girc's flood guard
	})
	m.installHandlers()
	return m
}

func (m *Manager) Nick() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nick != "" {
		return m.nick
	}
	return m.cfg.Nick
}

type Status struct {
	Connected      bool      `json:"connected"`
	Nick           string    `json:"nick"`
	LastSearchAt   time.Time `json:"last_search_at"`
	NextSearchAt   time.Time `json:"next_search_at"`
	PendingSearch  int       `json:"pending_searches"`
	PendingDownload int      `json:"pending_downloads"`
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		Connected:       m.connected,
		Nick:            m.nick,
		LastSearchAt:    m.lastSearchAt,
		NextSearchAt:    m.lastSearchAt.Add(m.cfg.SearchGap),
		PendingSearch:   len(m.pendingSearch),
		PendingDownload: len(m.pendingDLs),
	}
}

// QueueSearch submits a query. The manager will send "@<bot> <query>"
// to the channel as soon as the 10s rate limit allows.
func (m *Manager) QueueSearch(searchID, query string) {
	m.searchCh <- pendingSearch{id: searchID, query: query}
}

// QueueDownload sends the bot command immediately (no throttle) and
// remembers the downloadID so the eventual DCC SEND can be correlated
// back to it via the filename hint.
func (m *Manager) QueueDownload(downloadID, command string) error {
	if !m.client.IsConnected() {
		return errors.New("irc not connected")
	}
	hint := filenameHintFromCommand(command)
	m.mu.Lock()
	m.pendingDLs = append(m.pendingDLs, pendingDownload{
		id: downloadID, command: command, filenameHint: hint,
	})
	m.mu.Unlock()
	m.client.Cmd.Message(m.cfg.Channel, command)
	return nil
}

// Start blocks until ctx is cancelled, reconnecting on disconnect.
//
// girc.Client.Connect() blocks until the network closes — ctx cancellation
// alone won't unblock it. The watcher goroutine below force-closes the
// client on shutdown so this loop can exit promptly.
func (m *Manager) Start(ctx context.Context) {
	go m.searchSender(ctx)

	go func() {
		<-ctx.Done()
		m.client.Close()
	}()

	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		m.cfg.Logger.Printf("irc: dialing %s:%d as %s", m.cfg.Server, m.cfg.Port, m.cfg.Nick)
		if err := m.client.Connect(); err != nil {
			m.cfg.Logger.Printf("irc: connect error: %v", err)
		}
		m.mu.Lock()
		m.connected = false
		m.mu.Unlock()
		if m.cfg.Callbacks.OnDisconnected != nil {
			m.cfg.Callbacks.OnDisconnected()
		}
		if ctx.Err() != nil {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

func (m *Manager) searchSender(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ps := <-m.searchCh:
			m.mu.Lock()
			wait := time.Until(m.lastSearchAt.Add(m.cfg.SearchGap))
			m.mu.Unlock()
			if wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
			if !m.client.IsConnected() {
				if m.cfg.Callbacks.OnSearchFailed != nil {
					m.cfg.Callbacks.OnSearchFailed(ps.id, "irc not connected")
				}
				continue
			}
			m.mu.Lock()
			m.pendingSearch = append(m.pendingSearch, ps)
			m.lastSearchAt = time.Now()
			m.mu.Unlock()
			bot := strings.TrimPrefix(m.cfg.SearchBot, "@")
			m.client.Cmd.Message(m.cfg.Channel, fmt.Sprintf("@%s %s", bot, ps.query))
			m.cfg.Logger.Printf("irc: sent @%s %s (search_id=%s)", bot, ps.query, ps.id)
		}
	}
}

func (m *Manager) installHandlers() {
	m.client.Handlers.Add(girc.CONNECTED, func(c *girc.Client, _ girc.Event) {
		m.cfg.Logger.Printf("irc: connected as %s", c.GetNick())
		m.mu.Lock()
		m.connected = true
		m.nick = c.GetNick()
		m.mu.Unlock()
		// Server frequently sends a server NOTICE in the first ~2s; wait it out before joining.
		go func() {
			time.Sleep(connectAfterDelay)
			c.Cmd.Join(m.cfg.Channel)
			m.cfg.Logger.Printf("irc: joined %s", m.cfg.Channel)
			if m.cfg.Callbacks.OnConnected != nil {
				m.cfg.Callbacks.OnConnected(c.GetNick())
			}
		}()
	})

	m.client.Handlers.Add(girc.DISCONNECTED, func(c *girc.Client, _ girc.Event) {
		m.cfg.Logger.Printf("irc: disconnected")
		m.mu.Lock()
		m.connected = false
		m.mu.Unlock()
	})

	// Override girc's default VERSION reply with the irchighway-allowlisted UA.
	m.client.CTCP.Set(girc.CTCP_VERSION, func(c *girc.Client, ev girc.CTCPEvent) {
		c.Cmd.SendCTCPReply(ev.Source.Name, girc.CTCP_VERSION, m.cfg.UserAgent)
	})

	// CTCP DCC SEND — both search results and book downloads come this way.
	m.client.CTCP.Set("DCC", func(c *girc.Client, ev girc.CTCPEvent) {
		// ev.Text is "SEND filename ip port size".
		raw := "DCC " + ev.Text
		go m.handleDCC(raw)
	})

	m.client.Handlers.Add(girc.NOTICE, func(c *girc.Client, ev girc.Event) {
		text := ev.Last()
		switch {
		case strings.Contains(text, "Sorry"):
			m.failOldestSearch(text)
		case strings.Contains(text, "try another server"):
			m.failOldestDownload(text)
		case strings.Contains(text, "has been accepted"):
			id := m.peekOldestSearch()
			if id != "" && m.cfg.Callbacks.OnSearchAccepted != nil {
				m.cfg.Callbacks.OnSearchAccepted(id)
			}
		case strings.Contains(text, "matches"):
			n := parseMatchCount(text)
			id := m.peekOldestSearch()
			if id != "" && n >= 0 && m.cfg.Callbacks.OnSearchMatches != nil {
				m.cfg.Callbacks.OnSearchMatches(id, n)
			}
		}
	})
}

func (m *Manager) peekOldestSearch() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pendingSearch) == 0 {
		return ""
	}
	return m.pendingSearch[0].id
}

func (m *Manager) popOldestSearchByQueryHint(hint string) (pendingSearch, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pendingSearch) == 0 {
		return pendingSearch{}, false
	}
	// Best-effort: prefer one whose normalized query is a substring of the hint.
	if hint != "" {
		needle := strings.ToLower(hint)
		for i, p := range m.pendingSearch {
			if strings.Contains(needle, strings.ToLower(normalizeQuery(p.query))) {
				m.pendingSearch = append(m.pendingSearch[:i], m.pendingSearch[i+1:]...)
				return p, true
			}
		}
	}
	p := m.pendingSearch[0]
	m.pendingSearch = m.pendingSearch[1:]
	return p, true
}

func (m *Manager) popDownloadByFilename(filename string) (pendingDownload, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pendingDLs) == 0 {
		return pendingDownload{}, false
	}
	if filename != "" {
		needle := strings.ToLower(filename)
		for i, p := range m.pendingDLs {
			if p.filenameHint != "" && strings.Contains(needle, strings.ToLower(p.filenameHint)) {
				m.pendingDLs = append(m.pendingDLs[:i], m.pendingDLs[i+1:]...)
				return p, true
			}
		}
	}
	p := m.pendingDLs[0]
	m.pendingDLs = m.pendingDLs[1:]
	return p, true
}

func (m *Manager) failOldestSearch(msg string) {
	m.mu.Lock()
	if len(m.pendingSearch) == 0 {
		m.mu.Unlock()
		return
	}
	p := m.pendingSearch[0]
	m.pendingSearch = m.pendingSearch[1:]
	m.mu.Unlock()
	if m.cfg.Callbacks.OnSearchFailed != nil {
		m.cfg.Callbacks.OnSearchFailed(p.id, msg)
	}
}

func (m *Manager) failOldestDownload(msg string) {
	m.mu.Lock()
	if len(m.pendingDLs) == 0 {
		m.mu.Unlock()
		return
	}
	p := m.pendingDLs[0]
	m.pendingDLs = m.pendingDLs[1:]
	m.mu.Unlock()
	if m.cfg.Callbacks.OnDownloadFailed != nil {
		m.cfg.Callbacks.OnDownloadFailed(p.id, msg)
	}
}

func (m *Manager) handleDCC(raw string) {
	send, err := ParseDCCSend(raw)
	if err != nil {
		m.cfg.Logger.Printf("irc: dcc parse error: %v (%q)", err, raw)
		return
	}

	tmpPath := filepath.Join(m.cfg.WorkDir, sanitizeFilename(send.Filename))
	f, err := os.Create(tmpPath)
	if err != nil {
		m.cfg.Logger.Printf("irc: dcc create temp: %v", err)
		return
	}
	// keepFile is set true only when ownership of the temp file is handed
	// off (currently: only on successful book download where PocketBase
	// will copy and delete it). All other paths — search-result success,
	// search-result failure, orphan book, parse errors — leave keepFile
	// false and the deferred cleanup removes the temp file.
	closed := false
	keepFile := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		if !keepFile {
			_ = os.Remove(tmpPath)
		}
	}()

	isSearchResult := strings.Contains(send.Filename, "_results_for_")
	if !isSearchResult {
		pd, ok := m.popDownloadByFilename(send.Filename)
		if !ok {
			m.cfg.Logger.Printf("irc: unmatched book DCC SEND %s; discarding", send.Filename)
			_ = send.Download(f) // drain so the bot doesn't hang
			return
		}
		if m.cfg.Callbacks.OnDownloadStart != nil {
			m.cfg.Callbacks.OnDownloadStart(pd.id, send.Filename, send.Size)
		}
		if dlErr := send.Download(f); dlErr != nil {
			if m.cfg.Callbacks.OnDownloadFailed != nil {
				m.cfg.Callbacks.OnDownloadFailed(pd.id, dlErr.Error())
			}
			return
		}
		_ = f.Close()
		closed = true
		// Hand ownership of tmpPath to dccCompleteDownload — it (or the
		// PocketBase callback) becomes responsible for deletion.
		keepFile = true
		m.dccCompleteDownload(send, tmpPath, pd)
		return
	}

	if dlErr := send.Download(f); dlErr != nil {
		m.cfg.Logger.Printf("irc: dcc download (search) failed: %v", dlErr)
		if ps, ok := m.popOldestSearchByQueryHint(send.Filename); ok && m.cfg.Callbacks.OnSearchFailed != nil {
			m.cfg.Callbacks.OnSearchFailed(ps.id, dlErr.Error())
		}
		return
	}
	_ = f.Close()
	closed = true

	books, perrs, perr := readBooksFromArchiveOrText(tmpPath)
	if perr != nil {
		m.cfg.Logger.Printf("irc: parse search results: %v", perr)
		if ps, ok := m.popOldestSearchByQueryHint(send.Filename); ok && m.cfg.Callbacks.OnSearchFailed != nil {
			m.cfg.Callbacks.OnSearchFailed(ps.id, perr.Error())
		}
		return
	}
	if len(perrs) > 0 {
		m.cfg.Logger.Printf("irc: %d unparseable lines in search results", len(perrs))
	}

	ps, ok := m.popOldestSearchByQueryHint(send.Filename)
	if !ok {
		m.cfg.Logger.Printf("irc: search results arrived but no pending search to match")
		return
	}
	if m.cfg.Callbacks.OnSearchResults != nil {
		m.cfg.Callbacks.OnSearchResults(ps.id, books)
	}
}

// dccCompleteDownload runs after the bytes have already been written to
// tmpPath. It optionally extracts a zipped single-file archive, then
// hands the final file path to the OnDownloadDone callback. The callback
// (in main.go) is responsible for deleting the file once PocketBase has
// copied it into managed storage.
func (m *Manager) dccCompleteDownload(send *DCCSend, tmpPath string, pd pendingDownload) {
	finalPath := tmpPath
	finalName := send.Filename
	finalSize := send.Size
	if extracted, name, size, err := maybeExtractZip(tmpPath); err == nil && extracted != "" {
		_ = os.Remove(tmpPath)
		finalPath = extracted
		finalName = name
		finalSize = size
	}

	if m.cfg.Callbacks.OnDownloadDone != nil {
		m.cfg.Callbacks.OnDownloadDone(pd.id, finalPath, finalName, finalSize)
	} else {
		// No callback to take ownership; clean up so we don't leak.
		_ = os.Remove(finalPath)
	}
}

// readBooksFromArchiveOrText handles both raw .txt search results and
// the more common case of a .zip wrapping a .txt.
func readBooksFromArchiveOrText(path string) ([]BookResult, []ParseError, error) {
	zr, err := zip.OpenReader(path)
	if err == nil {
		defer zr.Close()
		for _, zf := range zr.File {
			if !strings.HasSuffix(strings.ToLower(zf.Name), ".txt") {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				return nil, nil, err
			}
			defer rc.Close()
			books, perrs := ParseSearch(rc)
			return books, perrs, nil
		}
		return nil, nil, errors.New("zip contained no .txt entry")
	}
	// Not a zip — treat as raw text.
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	books, perrs := ParseSearch(f)
	return books, perrs, nil
}

// maybeExtractZip — if path is a zip with exactly one inner file,
// extract it next to the zip and return its path. Otherwise returns ("","",0,nil).
func maybeExtractZip(path string) (string, string, int64, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", "", 0, nil
	}
	defer zr.Close()
	if len(zr.File) != 1 {
		return "", "", 0, nil
	}
	zf := zr.File[0]
	rc, err := zf.Open()
	if err != nil {
		return "", "", 0, err
	}
	defer rc.Close()

	outPath := filepath.Join(filepath.Dir(path), sanitizeFilename(zf.Name))
	out, err := os.Create(outPath)
	if err != nil {
		return "", "", 0, err
	}
	n, err := io.Copy(out, rc)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", "", 0, err
	}
	return outPath, filepath.Base(zf.Name), n, nil
}

func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	name = strings.ReplaceAll(name, string(filepath.Separator), "_")
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return name
}

// filenameHintFromCommand pulls a likely filename out of a download
// command like "!Oatmeal F. Scott Fitzgerald - The Great Gatsby.epub".
func filenameHintFromCommand(command string) string {
	idx := strings.Index(command, " ")
	if idx == -1 {
		return ""
	}
	rest := strings.TrimSpace(command[idx+1:])
	// The bot will typically wrap the file in a zip whose name doesn't
	// match `rest` exactly, so we use the trailing token (Title.epub)
	// as the matching needle.
	parts := strings.Split(rest, " ")
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.Contains(parts[i], ".") {
			return parts[i]
		}
	}
	return rest
}

func normalizeQuery(q string) string {
	// Bot encodes search query with spaces → underscores in the result filename.
	return strings.ReplaceAll(q, " ", "_")
}

// parseMatchCount extracts N from "...returned N matches".
func parseMatchCount(text string) int {
	idx := strings.LastIndex(text, "returned")
	if idx == -1 {
		return -1
	}
	end := strings.LastIndex(text, "matches")
	if end == -1 || end <= idx+len("returned ") {
		return -1
	}
	num := strings.TrimSpace(text[idx+len("returned ") : end])
	n := 0
	for _, c := range num {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
