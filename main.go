package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/x/term"
)

const (
	cloudflareAPIBase = "https://api.cloudflare.com/client/v4"
	defaultLookback   = 30 * 24 * time.Hour
)

// LogEntry contains the fields from an AI Gateway log needed for the report.
type LogEntry struct {
	ID        string            `json:"id"`
	CreatedAt time.Time         `json:"created_at"`
	Provider  string            `json:"provider"`
	Model     string            `json:"model"`
	Duration  int64             `json:"duration"`
	UserAgent string            `json:"user_agent"`
	TokensIn  int64             `json:"tokens_in"`
	TokensOut int64             `json:"tokens_out"`
	Cost      float64           `json:"cost"`
	Metadata  map[string]string `json:"metadata"`
}

type logsResponse struct {
	Success    bool       `json:"success"`
	Errors     []apiError `json:"errors"`
	Result     []LogEntry `json:"result"`
	ResultInfo struct {
		Page       int `json:"page"`
		PerPage    int `json:"per_page"`
		TotalCount int `json:"total_count"`
	} `json:"result_info"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type totals struct {
	Requests  int
	Sessions  int
	Duration  int64
	TokensIn  int64
	TokensOut int64
	Cost      float64
}

type sessionModelGroup struct {
	SessionID  string
	Model      string
	Models     map[string]struct{}
	UserAgents map[string]struct{}
	Metadata   map[string]map[string]struct{}
	FirstSeen  time.Time
	LastSeen   time.Time
	Totals     totals
}

type reportColumn struct {
	Label string `json:"label"`
	Key   string `json:"key"`
}

type columnFlags []reportColumn

func (columns *columnFlags) String() string {
	values := make([]string, len(*columns))
	for i, column := range *columns {
		values[i] = column.Label + ":" + column.Key
	}
	return strings.Join(values, ",")
}

func (columns *columnFlags) Set(value string) error {
	label, key, found := strings.Cut(value, ":")
	label, key = strings.TrimSpace(label), strings.TrimSpace(key)
	if !found || label == "" || key == "" {
		return fmt.Errorf("invalid --column %q; use label:metadata.key", value)
	}
	*columns = append(*columns, reportColumn{Label: label, Key: key})
	return nil
}

type defaultSettings struct {
	Mode    string         `json:"mode"`
	Columns []reportColumn `json:"columns,omitempty"`
	Daily   bool           `json:"daily,omitempty"`
	All     bool           `json:"all,omitempty"`
	Tokens  bool           `json:"tokens,omitempty"`
	UA      bool           `json:"ua,omitempty"`
	Join    bool           `json:"join,omitempty"`
	Raw     bool           `json:"raw,omitempty"`
	JSON    bool           `json:"json,omitempty"`
	UTC     bool           `json:"utc,omitempty"`
	Today   bool           `json:"today,omitempty"`
}

type savedConfig struct {
	AccountID string          `json:"account_id"`
	Gateway   string          `json:"gateway"`
	UserID    string          `json:"user_id"`
	Defaults  defaultSettings `json:"defaults"`
}

type config struct {
	accountID    string
	gateway      string
	userID       string
	start        time.Time
	end          time.Time
	daily        bool
	allDaily     bool
	showTokens   bool
	showUA       bool
	joinSessions bool
	session      string
	force        bool
	raw          bool
	json         bool
	utc          bool
	today        bool
	columns      []reportColumn
	durationSet  bool
	fetchLatency time.Duration
	fetched      bool
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		if len(os.Args) > 2 {
			fmt.Fprintln(os.Stderr, "error: version takes no arguments")
			os.Exit(2)
		}
		printVersion()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		if err := runSetup(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "set-defaults" {
		if err := runSetDefaults(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "defaults" {
		if len(os.Args) > 2 {
			fmt.Fprintln(os.Stderr, "error: defaults takes no arguments")
			os.Exit(2)
		}
		settings, err := loadConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println(defaultsFlags(settings.Defaults))
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "set-token" {
		if err := runSetToken(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	settings, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	cfg, err := parseFlags(os.Args[1:], settings.Defaults)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	settings, err = ensureConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	cfg.accountID, cfg.gateway = settings.AccountID, settings.Gateway
	if cfg.userID == "" {
		cfg.userID = settings.UserID
	}

	entries, piped, err := readPipedEntries(cfg.userID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error reading JSON from stdin:", err)
		os.Exit(1)
	}
	if !piped {
		token, tokenErr := newTokenStore().Load(cfg.accountID, cfg.gateway)
		if tokenErr != nil {
			fmt.Fprintln(os.Stderr, "error:", tokenErr)
			os.Exit(1)
		}
		entries, cfg.fetchLatency, err = fetchWithCache(&http.Client{Timeout: time.Minute}, cfg, token)
		cfg.fetched = true
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}

	if piped && cfg.durationSet {
		entries = filterEntriesByTime(entries, cfg.start, cfg.end)
	}
	if cfg.session != "" {
		entries = filterEntriesForSession(entries, cfg.session)
		if len(entries) == 0 {
			fmt.Fprintf(os.Stderr, "error: session %q not found\n", cfg.session)
			os.Exit(1)
		}
	}
	markdown := report(entries, cfg, piped)
	if cfg.json {
		output, err := reportJSON(entries, cfg, piped)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error encoding report:", err)
			os.Exit(1)
		}
		fmt.Println(string(output))
		return
	}
	if cfg.raw {
		fmt.Print(markdown)
		return
	}
	rendered, err := renderReport(markdown)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error rendering report:", err)
		os.Exit(1)
	}
	fmt.Print(rendered)
}

func printVersion() {
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		return
	}
	version := strings.TrimSpace(info.Main.Version)
	if version != "" && version != "(devel)" {
		fmt.Println(version)
		return
	}

	revision := ""
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = strings.TrimSpace(setting.Value)
		case "vcs.modified":
			modified = strings.EqualFold(strings.TrimSpace(setting.Value), "true")
		}
	}
	if revision == "" {
		return
	}
	if modified {
		revision += "-dirty"
	}
	fmt.Println(revision)
}

func runSetup() error {
	settings, err := loadConfig()
	if err != nil {
		return err
	}
	settings, err = promptConfig(settings, false)
	if err != nil {
		return err
	}
	if err := saveConfig(settings); err != nil {
		return err
	}
	return promptAndSaveToken(settings, true)
}

func runSetToken() error {
	settings, err := ensureConfig()
	if err != nil {
		return err
	}
	return promptAndSaveToken(settings, false)
}

func promptAndSaveToken(settings savedConfig, allowKeep bool) error {
	store := newTokenStore()
	exists, err := store.Exists(settings.AccountID, settings.Gateway)
	if err != nil {
		return err
	}
	if allowKeep && exists {
		fmt.Fprint(os.Stderr, "Cloudflare API token [stored; press Enter to keep]: ")
	} else {
		fmt.Fprint(os.Stderr, "Cloudflare API token: ")
	}
	token, err := term.ReadPassword(os.Stdin.Fd())
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	if strings.TrimSpace(string(token)) == "" && allowKeep && exists {
		return nil
	}
	if strings.TrimSpace(string(token)) == "" {
		return errors.New("token is required")
	}
	return store.Save(settings.AccountID, settings.Gateway, string(token))
}

func runSetDefaults(args []string) error {
	settings, err := loadConfig()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		settings.Defaults = defaultSettings{}
		return saveConfig(settings)
	}
	defaults, err := parseDefaultSettings(args)
	if err != nil {
		return err
	}
	settings.Defaults = defaults
	return saveConfig(settings)
}

func parseDefaultSettings(args []string) (defaultSettings, error) {
	var defaults defaultSettings
	fs := flag.NewFlagSet("set-defaults", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.StringVar(&defaults.Mode, "mode", "default", "default behavior: default or base")
	fs.StringVar(&defaults.Mode, "m", "default", "shorthand for --mode")
	fs.BoolVar(&defaults.Daily, "daily", false, "include daily usage")
	fs.BoolVar(&defaults.All, "all", false, "include daily usage tables per model")
	fs.BoolVar(&defaults.Tokens, "tokens", false, "include token columns")
	fs.BoolVar(&defaults.UA, "ua", false, "include user-agent column")
	fs.BoolVar(&defaults.Join, "join", false, "combine models used in a session")
	fs.BoolVar(&defaults.Raw, "raw", false, "write raw Markdown")
	fs.BoolVar(&defaults.JSON, "json", false, "write a single-line JSON report")
	fs.BoolVar(&defaults.UTC, "utc", false, "display dates and times in UTC")
	fs.BoolVar(&defaults.Today, "today", false, "use the range from the start of today until now")
	fs.Var((*columnFlags)(&defaults.Columns), "column", "add table column as label:metadata.key (repeatable)")
	if err := fs.Parse(args); err != nil {
		return defaults, err
	}
	if fs.NArg() != 0 {
		return defaults, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	defaults.Mode = strings.ToLower(strings.TrimSpace(defaults.Mode))
	if defaults.Mode != "default" && defaults.Mode != "base" {
		return defaults, fmt.Errorf("invalid --mode %q; use default or base", defaults.Mode)
	}
	if defaults.All && !defaults.Daily {
		return defaults, errors.New("--all requires --daily")
	}
	return defaults, nil
}

func ensureConfig() (savedConfig, error) {
	settings, err := loadConfig()
	if err != nil {
		return savedConfig{}, err
	}
	if settings.AccountID != "" && settings.Gateway != "" && settings.UserID != "" {
		return settings, nil
	}
	info, err := os.Stdin.Stat()
	if err != nil {
		return savedConfig{}, err
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return savedConfig{}, errors.New("configuration is incomplete; run cfaicost setup before piping JSON")
	}
	settings, err = promptConfig(settings, true)
	if err != nil {
		return savedConfig{}, err
	}
	if err := saveConfig(settings); err != nil {
		return savedConfig{}, err
	}
	return settings, nil
}

func configPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config directory: %w", err)
	}
	return filepath.Join(base, "cfaicost", "config.json"), nil
}

func loadConfig() (savedConfig, error) {
	path, err := configPath()
	if err != nil {
		return savedConfig{}, err
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return savedConfig{}, nil
	}
	if err != nil {
		return savedConfig{}, fmt.Errorf("read config: %w", err)
	}
	var settings savedConfig
	if err := json.Unmarshal(contents, &settings); err != nil {
		return savedConfig{}, fmt.Errorf("decode config: %w", err)
	}
	return settings, nil
}

func saveConfig(settings savedConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	contents, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	contents = append(contents, '\n')
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, contents, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("commit config: %w", err)
	}
	return nil
}

func promptConfig(settings savedConfig, missingOnly bool) (savedConfig, error) {
	reader := bufio.NewReader(os.Stdin)
	var err error
	if !missingOnly || settings.AccountID == "" {
		settings.AccountID, err = promptValue(reader, "Account ID", settings.AccountID)
		if err != nil {
			return settings, err
		}
	}
	if !missingOnly || settings.Gateway == "" {
		settings.Gateway, err = promptValue(reader, "Gateway", settings.Gateway)
		if err != nil {
			return settings, err
		}
	}
	if !missingOnly || settings.UserID == "" {
		settings.UserID, err = promptValue(reader, "User ID", settings.UserID)
		if err != nil {
			return settings, err
		}
	}
	return settings, nil
}

func promptValue(reader *bufio.Reader, label, current string) (string, error) {
	for {
		if current == "" {
			fmt.Printf("%s: ", label)
		} else {
			fmt.Printf("%s [%s]: ", label, current)
		}
		value, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		value = strings.TrimSpace(value)
		if value != "" {
			return value, nil
		}
		if current != "" {
			return current, nil
		}
		if errors.Is(err, io.EOF) {
			return "", errors.New("configuration value is required")
		}
		fmt.Fprintln(os.Stderr, "A value is required.")
	}
}

func renderReport(markdown string) (string, error) {
	options := []glamour.TermRendererOption{glamour.WithStandardStyle("dark")}
	if width, _, err := term.GetSize(os.Stdout.Fd()); err == nil && width > 0 {
		options = append(options, glamour.WithWordWrap(width))
	}
	renderer, err := glamour.NewTermRenderer(options...)
	if err != nil {
		return "", err
	}
	return renderer.Render(markdown)
}

func parseFlags(args []string, defaults defaultSettings) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("cfaicost", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: cfaicost [flags] | cfaicost setup | cfaicost set-token | cfaicost defaults | cfaicost version")
		fmt.Fprintln(fs.Output(), "Fetch or render a Cloudflare AI Gateway cost report.")
		fmt.Fprintln(fs.Output(), "\nFlags:")
		fs.PrintDefaults()
	}
	var start, end, duration string
	fs.StringVar(&start, "start", "", "inclusive start time (RFC3339)")
	fs.StringVar(&end, "end", "", "exclusive end time (RFC3339)")
	fs.StringVar(&duration, "duration", "", "lookback duration (for example 7d or 168h)")
	fs.StringVar(&duration, "d", "", "shorthand for --duration")
	fs.StringVar(&cfg.userID, "user", "", "fetch the report for this user ID")
	fs.StringVar(&cfg.userID, "u", "", "shorthand for --user")
	fs.BoolVar(&cfg.daily, "daily", false, "include daily usage")
	fs.BoolVar(&cfg.allDaily, "all", false, "include daily usage tables per model (requires --daily)")
	fs.BoolVar(&cfg.showTokens, "tokens", false, "include token columns")
	fs.BoolVar(&cfg.showUA, "ua", false, "include user-agent column")
	fs.BoolVar(&cfg.joinSessions, "join", false, "combine all models used in each session")
	fs.StringVar(&cfg.session, "session", "", "show only the specified session ID")
	fs.StringVar(&cfg.session, "s", "", "shorthand for --session")
	fs.BoolVar(&cfg.force, "force", false, "refetch data instead of using cached days")
	fs.BoolVar(&cfg.force, "f", false, "shorthand for --force")
	fs.BoolVar(&cfg.raw, "raw", false, "write raw Markdown instead of Glamour-rendered output")
	fs.BoolVar(&cfg.json, "json", false, "write a single-line JSON report")
	fs.BoolVar(&cfg.utc, "utc", false, "display dates and times in UTC")
	fs.BoolVar(&cfg.today, "today", false, "use the range from the start of today until now")
	fs.Var((*columnFlags)(&cfg.columns), "column", "add table column as label:metadata.key (repeatable)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() != 0 {
		return cfg, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if cfg.session == "" && (flagWasSet(fs, "session") || flagWasSet(fs, "s")) {
		return cfg, errors.New("--session requires a session ID")
	}
	if cfg.userID == "" && flagWasSet(fs, "user") {
		return cfg, errors.New("--user requires a user ID")
	}
	explicit := make(map[string]bool)
	forEachFlag := func(f *flag.Flag) { explicit[f.Name] = true }
	fs.Visit(forEachFlag)
	useDefaults := !hasNonForceFlag(explicit) || strings.EqualFold(defaults.Mode, "base")
	if useDefaults {
		applyDefaults(&cfg, defaults, explicit)
	}
	if cfg.allDaily && !cfg.daily {
		return cfg, errors.New("--all requires --daily")
	}
	now := time.Now().UTC()
	if cfg.today {
		localNow := now.In(time.Local)
		cfg.durationSet = true
		cfg.start = time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.Local)
		cfg.end = now
		return cfg, nil
	}
	if duration != "" {
		cfg.durationSet = true
		lookback, err := parseLookback(duration)
		if err != nil {
			return cfg, err
		}
		cfg.end = now
		cfg.start = now.Add(-lookback)
		return cfg, nil
	}
	var err error
	if start == "" {
		cfg.start = now.Add(-defaultLookback)
	} else if cfg.start, err = time.Parse(time.RFC3339Nano, start); err != nil {
		return cfg, fmt.Errorf("invalid --start: %w", err)
	}
	if end == "" {
		cfg.end = now
	} else if cfg.end, err = time.Parse(time.RFC3339Nano, end); err != nil {
		return cfg, fmt.Errorf("invalid --end: %w", err)
	}
	if !cfg.end.After(cfg.start) {
		return cfg, errors.New("--end must be after --start")
	}
	return cfg, nil
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func hasNonForceFlag(explicit map[string]bool) bool {
	for name := range explicit {
		if name != "force" && name != "f" && name != "session" && name != "s" && name != "user" && name != "u" {
			return true
		}
	}
	return false
}

func applyDefaults(cfg *config, defaults defaultSettings, explicit map[string]bool) {
	if !explicit["daily"] {
		cfg.daily = defaults.Daily
	}
	if !explicit["all"] {
		cfg.allDaily = defaults.All
	}
	if !explicit["tokens"] {
		cfg.showTokens = defaults.Tokens
	}
	if !explicit["ua"] {
		cfg.showUA = defaults.UA
	}
	if !explicit["join"] {
		cfg.joinSessions = defaults.Join
	}
	if !explicit["raw"] {
		cfg.raw = defaults.Raw
	}
	if !explicit["json"] {
		cfg.json = defaults.JSON
	}
	if !explicit["utc"] {
		cfg.utc = defaults.UTC
	}
	if !explicit["today"] {
		cfg.today = defaults.Today
	}
	if len(defaults.Columns) > 0 {
		cfg.columns = append(append([]reportColumn(nil), defaults.Columns...), cfg.columns...)
	}
}

func parseLookback(value string) (time.Duration, error) {
	original := value
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(value, "d"), 64)
		lookback := time.Duration(days * float64(24*time.Hour))
		if err == nil && days > 0 && lookback > 0 {
			return lookback, nil
		}
		return 0, fmt.Errorf("invalid --duration %q; use a positive duration such as 7d or 168h", original)
	}
	lookback, err := time.ParseDuration(value)
	if err != nil || lookback <= 0 {
		return 0, fmt.Errorf("invalid --duration %q; use a positive duration such as 7d or 168h", original)
	}
	return lookback, nil
}

// fetchWithCache reuses complete UTC days before today and fetches only the gaps.
func fetchWithCache(client *http.Client, cfg config, token string) ([]LogEntry, time.Duration, error) {
	cache, err := newCache(cfg)
	if err != nil {
		return nil, 0, err
	}

	var entries, fetchedEntries []LogEntry
	var fetchStart time.Time
	missingDays := make(map[string]struct{})
	var latency time.Duration
	today := utcDay(time.Now())
	for day := utcDay(cfg.start); day.Before(cfg.end); day = day.AddDate(0, 0, 1) {
		dayEnd := day.AddDate(0, 0, 1)
		start, end := maxTime(cfg.start, day), minTime(cfg.end, dayEnd)
		cacheable := start.Equal(day) && end.Equal(dayEnd) && day.Before(today)
		if cacheable && !cfg.force {
			cached, found, err := cache.read(day)
			if err != nil {
				return nil, latency, err
			}
			if found {
				if !fetchStart.IsZero() {
					fetched, took, err := fetchLogs(client, configWithRange(cfg, fetchStart, start), token)
					if err != nil {
						return nil, latency, err
					}
					entries, fetchedEntries, latency = append(entries, fetched...), append(fetchedEntries, fetched...), latency+took
					fetchStart = time.Time{}
				}
				entries = append(entries, cached...)
				continue
			}
		}
		if cacheable {
			missingDays[day.Format("2006-01-02")] = struct{}{}
		}
		if fetchStart.IsZero() {
			fetchStart = start
		}
	}
	if !fetchStart.IsZero() {
		fetched, took, err := fetchLogs(client, configWithRange(cfg, fetchStart, cfg.end), token)
		if err != nil {
			return nil, latency, err
		}
		entries, fetchedEntries, latency = append(entries, fetched...), append(fetchedEntries, fetched...), latency+took
	}
	for date := range missingDays {
		day, _ := time.Parse("2006-01-02", date)
		if err := cache.write(day, entriesForDay(fetchedEntries, day)); err != nil {
			return nil, latency, err
		}
	}
	return filterEntriesByTime(entries, cfg.start, cfg.end), latency, nil
}

type dayCache struct {
	dir     string
	account string
	gateway string
	userID  string
}

func newCache(cfg config) (dayCache, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return dayCache{}, fmt.Errorf("locate cache directory: %w", err)
	}
	cache := dayCache{dir: base + "/cfaicost", account: cfg.accountID, gateway: cfg.gateway, userID: cfg.userID}
	if err := os.MkdirAll(cache.dir, 0o700); err != nil {
		return dayCache{}, fmt.Errorf("create cache directory: %w", err)
	}
	return cache, nil
}

func (cache dayCache) path(day time.Time) string {
	identity := cache.account + "\x00" + cache.gateway + "\x00" + cache.userID
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s/%x--%s.json", cache.dir, digest, day.UTC().Format("2006-01-02"))
}

func (cache dayCache) read(day time.Time) ([]LogEntry, bool, error) {
	contents, err := os.ReadFile(cache.path(day))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read cache: %w", err)
	}
	var entries []LogEntry
	if err := json.Unmarshal(contents, &entries); err != nil {
		return nil, false, fmt.Errorf("decode cache %s: %w", cache.path(day), err)
	}
	return entries, true, nil
}

func (cache dayCache) write(day time.Time, entries []LogEntry) error {
	if !day.Before(utcDay(time.Now())) {
		return nil
	}
	contents, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("encode cache: %w", err)
	}
	temporary := cache.path(day) + ".tmp"
	if err := os.WriteFile(temporary, contents, 0o600); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	if err := os.Rename(temporary, cache.path(day)); err != nil {
		return fmt.Errorf("commit cache: %w", err)
	}
	return nil
}

func entriesForDay(entries []LogEntry, day time.Time) []LogEntry {
	return filterEntriesByTime(entries, day, day.AddDate(0, 0, 1))
}

func configWithRange(cfg config, start, end time.Time) config {
	cfg.start, cfg.end = start, end
	return cfg
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func utcDay(value time.Time) time.Time {
	return time.Date(value.UTC().Year(), value.UTC().Month(), value.UTC().Day(), 0, 0, 0, 0, time.UTC)
}

func fetchLogs(client *http.Client, cfg config, token string) ([]LogEntry, time.Duration, error) {
	if strings.TrimSpace(token) == "" {
		return nil, 0, errors.New("Cloudflare API token is empty")
	}
	started := time.Now()
	var all []LogEntry
	for page := 1; ; page++ {
		u, err := logsURL(cfg, page)
		if err != nil {
			return nil, 0, err
		}
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, 0, readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, 0, fmt.Errorf("Cloudflare API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		var payload logsResponse
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, 0, fmt.Errorf("decode Cloudflare response: %w", err)
		}
		if !payload.Success {
			return nil, 0, fmt.Errorf("Cloudflare API request failed: %v", payload.Errors)
		}
		all = append(all, payload.Result...)
		if len(payload.Result) == 0 || len(all) >= payload.ResultInfo.TotalCount || page*payload.ResultInfo.PerPage >= payload.ResultInfo.TotalCount {
			break
		}
	}
	return filterEntriesForUser(all, cfg.userID), time.Since(started), nil
}

func readPipedEntries(userID string) ([]LogEntry, bool, error) {
	info, err := os.Stdin.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return nil, false, nil
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, true, err
	}
	input = bytes.TrimSpace(input)
	if len(input) == 0 {
		return nil, false, nil
	}
	var entries []LogEntry
	if input[0] == '[' {
		err = json.Unmarshal(input, &entries)
	} else {
		var payload logsResponse
		err = json.Unmarshal(input, &payload)
		entries = payload.Result
	}
	if err != nil {
		return nil, true, err
	}
	return filterEntriesForUser(entries, userID), true, nil
}

func filterEntriesForUser(entries []LogEntry, userID string) []LogEntry {
	users := make(map[string]struct{})
	for _, entry := range entries {
		users[entry.Metadata["cf.user_id"]] = struct{}{}
	}
	if len(users) <= 1 {
		return entries
	}
	filtered := make([]LogEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Metadata["cf.user_id"] == userID {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func logsURL(cfg config, page int) (string, error) {
	base, err := url.Parse(cloudflareAPIBase)
	if err != nil {
		return "", err
	}
	base.Path += "/accounts/" + url.PathEscape(cfg.accountID) + "/ai-gateway/gateways/" + url.PathEscape(cfg.gateway) + "/logs"
	filters := []map[string]any{
		{"key": "metadata.key", "operator": "eq", "value": []string{"cf.user_id"}},
		{"key": "metadata.value", "operator": "eq", "value": []string{cfg.userID}},
		{"key": "created_at", "operator": "gt", "value": []string{cfg.start.UTC().Format(time.RFC3339Nano)}},
		{"key": "created_at", "operator": "lt", "value": []string{cfg.end.UTC().Format(time.RFC3339Nano)}},
	}
	encodedFilters, err := json.Marshal(filters)
	if err != nil {
		return "", err
	}
	q := base.Query()
	q.Set("order_by", "created_at")
	q.Set("order_by_direction", "desc")
	q.Set("page", fmt.Sprint(page))
	q.Set("per_page", "50")
	q.Set("filters", string(encodedFilters))
	base.RawQuery = q.Encode()
	return base.String(), nil
}

type jsonReport struct {
	User          string           `json:"user"`
	DateRange     string           `json:"date_range"`
	Requests      int              `json:"requests"`
	Sessions      int              `json:"sessions"`
	FetchLatency  string           `json:"fetch_latency"`
	TokensIn      *int64           `json:"tokens_in,omitempty"`
	TokensOut     *int64           `json:"tokens_out,omitempty"`
	Cost          float64          `json:"cost"`
	Overview      []map[string]any `json:"overview"`
	TotalsByModel []map[string]any `json:"totals_by_model"`
	DailyUsage    []map[string]any `json:"daily_usage,omitempty"`
	DailyByModel  []map[string]any `json:"daily_usage_by_model,omitempty"`
}

func reportJSON(entries []LogEntry, cfg config, piped bool) ([]byte, error) {
	total := sum(entries)
	location := time.Local
	if cfg.utc {
		location = time.UTC
	}
	start, end := cfg.start, cfg.end
	if piped && !cfg.durationSet {
		start, end = entryRange(entries)
	}
	result := jsonReport{User: cfg.userID, DateRange: formatDateRange(start, end, location), Requests: total.Requests, Sessions: total.Sessions, Cost: total.Cost, Overview: []map[string]any{}, TotalsByModel: []map[string]any{}}
	if cfg.showTokens {
		result.TokensIn = &total.TokensIn
		result.TokensOut = &total.TokensOut
	}
	if cfg.fetched {
		result.FetchLatency = cfg.fetchLatency.String()
	} else {
		result.FetchLatency = "n/a (stdin)"
	}
	for _, group := range sessionModelGroups(entries, cfg.joinSessions) {
		row := map[string]any{"period": formatPeriod(group.FirstSeen, group.LastSeen, time.Duration(group.Totals.Duration)*time.Millisecond, location), "model": group.Model, "requests": group.Totals.Requests, "cost": group.Totals.Cost}
		if hasSessionIDs(entries) {
			row["session"] = group.SessionID
		}
		if cfg.showUA {
			row["ua"] = joinSet(group.UserAgents)
		}
		for _, column := range cfg.columns {
			row[column.Label] = joinSet(group.Metadata[column.Key])
		}
		if cfg.showTokens {
			row["tokens_in"], row["tokens_out"] = group.Totals.TokensIn, group.Totals.TokensOut
		}
		result.Overview = append(result.Overview, row)
	}
	byModel := group(entries, modelName)
	if cfg.daily {
		result.DailyUsage = []map[string]any{}
	}
	if cfg.allDaily {
		result.DailyByModel = []map[string]any{}
	}
	appendTotals := func(target *[]map[string]any, name string, value totals) {
		row := map[string]any{"name": name, "requests": value.Requests, "sessions": value.Sessions, "cost": value.Cost}
		if cfg.showTokens {
			row["tokens_in"], row["tokens_out"] = value.TokensIn, value.TokensOut
		}
		*target = append(*target, row)
	}
	models := make([]string, 0, len(byModel))
	for model := range byModel {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		appendTotals(&result.TotalsByModel, model, byModel[model])
	}
	if cfg.daily {
		for _, date := range sortedGroupKeys(group(entries, func(e LogEntry) string { return e.CreatedAt.In(location).Format("2006-01-02") })) {
			appendTotals(&result.DailyUsage, date, group(entries, func(e LogEntry) string { return e.CreatedAt.In(location).Format("2006-01-02") })[date])
		}
	}
	if cfg.allDaily {
		for _, model := range models {
			days := group(filterModel(entries, model), func(e LogEntry) string { return e.CreatedAt.In(location).Format("2006-01-02") })
			for _, date := range sortedGroupKeys(days) {
				appendTotals(&result.DailyByModel, model+"/"+date, days[date])
			}
		}
	}
	return json.Marshal(result)
}

func sortedGroupKeys(groups map[string]totals) []string {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func defaultsFlags(defaults defaultSettings) string {
	flags := []string{}
	if defaults.Daily {
		flags = append(flags, "--daily")
	}
	if defaults.All {
		flags = append(flags, "--all")
	}
	if defaults.Tokens {
		flags = append(flags, "--tokens")
	}
	if defaults.UA {
		flags = append(flags, "--ua")
	}
	if defaults.Join {
		flags = append(flags, "--join")
	}
	if defaults.Raw {
		flags = append(flags, "--raw")
	}
	if defaults.JSON {
		flags = append(flags, "--json")
	}
	if defaults.UTC {
		flags = append(flags, "--utc")
	}
	if defaults.Today {
		flags = append(flags, "--today")
	}
	for _, column := range defaults.Columns {
		flags = append(flags, "--column="+column.Label+":"+column.Key)
	}
	return strings.Join(flags, " ")
}

func report(entries []LogEntry, cfg config, piped bool) string {
	var out bytes.Buffer
	total := sum(entries)
	location := time.Local
	if cfg.utc {
		location = time.UTC
	}
	start, end := cfg.start, cfg.end
	if piped && !cfg.durationSet {
		start, end = entryRange(entries)
	}
	fmt.Fprintln(&out, "# Cloudflare AI Gateway cost report")
	fmt.Fprintln(&out, "\n## Summary")
	fmt.Fprintf(&out, "- **User:** %s\n- **Date range:** %s\n- **Requests:** %d\n- **Sessions:** %d\n", cell(cfg.userID), formatDateRange(start, end, location), total.Requests, total.Sessions)
	if cfg.fetched {
		fmt.Fprintf(&out, "- **Fetch latency:** %s\n", cfg.fetchLatency)
	} else {
		fmt.Fprintln(&out, "- **Fetch latency:** n/a (stdin)")
	}
	if cfg.showTokens {
		fmt.Fprintf(&out, "- **Tokens in:** %d\n- **Tokens out:** %d\n", total.TokensIn, total.TokensOut)
	}
	fmt.Fprintf(&out, "- **Cost:** $%.6f\n", total.Cost)

	fmt.Fprintln(&out, "\n## Overview")
	showSession := hasSessionIDs(entries)
	writeRequestHeader(&out, showSession, cfg.showTokens, cfg.showUA, cfg.columns)
	for _, group := range sessionModelGroups(entries, cfg.joinSessions) {
		writeRequestRow(&out, group, showSession, cfg.showTokens, cfg.showUA, cfg.columns, location)
	}

	fmt.Fprintln(&out, "\n## Totals by model")
	byModel := group(entries, modelName)
	writeTotalsHeader(&out, "Model", cfg.showTokens)
	writeTotals(&out, byModel, cfg.showTokens)

	if cfg.daily {
		fmt.Fprintln(&out, "\n## Daily usage (all models)")
		writeTotalsHeader(&out, "Date", cfg.showTokens)
		writeTotals(&out, group(entries, func(e LogEntry) string { return e.CreatedAt.In(location).Format("2006-01-02") }), cfg.showTokens)
	}
	if cfg.allDaily {
		models := make([]string, 0, len(byModel))
		for model := range byModel {
			models = append(models, model)
		}
		sort.Strings(models)
		for _, model := range models {
			modelEntries := filterModel(entries, model)
			fmt.Fprintf(&out, "\n## Daily usage: %s\n", cell(model))
			writeTotalsHeader(&out, "Date", cfg.showTokens)
			writeTotals(&out, group(modelEntries, func(e LogEntry) string { return e.CreatedAt.In(location).Format("2006-01-02") }), cfg.showTokens)
			writeTotalRow(&out, sum(modelEntries), cfg.showTokens)
		}
	}
	return out.String()
}

func writeRequestHeader(out *bytes.Buffer, session, tokens, userAgent bool, columns []reportColumn) {
	headers := []string{"Period", "Model", "Requests"}
	align := []string{"---", "---", "---:"}
	if session {
		headers = append([]string{"Session"}, headers...)
		align = append([]string{"---"}, align...)
	}
	if userAgent {
		headers = append(headers, "UA")
		align = append(align, "---")
	}
	for _, column := range columns {
		headers = append(headers, cell(column.Label))
		align = append(align, "---")
	}
	if tokens {
		headers = append(headers, "Tokens In", "Tokens Out")
		align = append(align, "---:", "---:")
	}
	headers = append(headers, "Cost ($)")
	align = append(align, "---:")
	fmt.Fprintf(out, "| %s |\n| %s |\n", strings.Join(headers, " | "), strings.Join(align, " | "))
}

func writeRequestRow(out *bytes.Buffer, group sessionModelGroup, session, tokens, userAgent bool, columns []reportColumn, location *time.Location) {
	values := []string{formatPeriod(group.FirstSeen, group.LastSeen, time.Duration(group.Totals.Duration)*time.Millisecond, location), cell(group.Model), fmt.Sprint(group.Totals.Requests)}
	if session {
		values = append([]string{cell(group.SessionID)}, values...)
	}
	if userAgent {
		values = append(values, cell(joinSet(group.UserAgents)))
	}
	for _, column := range columns {
		values = append(values, cell(joinSet(group.Metadata[column.Key])))
	}
	if tokens {
		values = append(values, fmt.Sprint(group.Totals.TokensIn), fmt.Sprint(group.Totals.TokensOut))
	}
	values = append(values, fmt.Sprintf("%.6f", group.Totals.Cost))
	fmt.Fprintf(out, "| %s |\n", strings.Join(values, " | "))
}

func writeTotalsHeader(out *bytes.Buffer, firstColumn string, tokens bool) {
	if tokens {
		fmt.Fprintf(out, "| %s | Requests | Sessions | Tokens In | Tokens Out | Cost ($) |\n| --- | ---: | ---: | ---: | ---: | ---: |\n", firstColumn)
		return
	}
	fmt.Fprintf(out, "| %s | Requests | Sessions | Cost ($) |\n| --- | ---: | ---: | ---: |\n", firstColumn)
}

func writeTotalRow(out *bytes.Buffer, total totals, tokens bool) {
	if tokens {
		fmt.Fprintf(out, "| **Total** | **%d** | **%d** | **%d** | **%d** | **%.6f** |\n", total.Requests, total.Sessions, total.TokensIn, total.TokensOut, total.Cost)
		return
	}
	fmt.Fprintf(out, "| **Total** | **%d** | **%d** | **%.6f** |\n", total.Requests, total.Sessions, total.Cost)
}

func formatDateRange(start, end time.Time, location *time.Location) string {
	if start.IsZero() || end.IsZero() {
		return "n/a"
	}
	start, end = start.In(location), end.In(location)
	if start.Format("2006-01-02") == end.Format("2006-01-02") {
		return fmt.Sprintf("%s %s–%s", start.Format("2006-01-02"), start.Format("15:04:05"), end.Format("15:04:05"))
	}
	return fmt.Sprintf("%s %s–%s %s", start.Format("2006-01-02"), start.Format("15:04:05"), end.Format("2006-01-02"), end.Format("15:04:05"))
}

func formatPeriod(start, end time.Time, duration time.Duration, location *time.Location) string {
	return formatDateRange(start, end, location) + " (" + duration.String() + ")"
}

func entryRange(entries []LogEntry) (time.Time, time.Time) {
	if len(entries) == 0 {
		return time.Time{}, time.Time{}
	}
	start, end := entries[0].CreatedAt, entries[0].CreatedAt
	for _, entry := range entries[1:] {
		if entry.CreatedAt.Before(start) {
			start = entry.CreatedAt
		}
		if entry.CreatedAt.After(end) {
			end = entry.CreatedAt
		}
	}
	return start, end
}

func sessionCount(entries []LogEntry) int {
	sessions := make(map[string]struct{})
	for _, entry := range entries {
		sessions[metadataValue(entry.Metadata, "x-session-id")] = struct{}{}
	}
	return len(sessions)
}

func metadataValue(metadata map[string]string, key string) string {
	for candidate, value := range metadata {
		if strings.EqualFold(candidate, key) {
			return value
		}
	}
	return ""
}

func modelName(entry LogEntry) string { return entry.Provider + "/" + entry.Model }

func sessionModelGroups(entries []LogEntry, joinSessions bool) []sessionModelGroup {
	groups := make(map[string]*sessionModelGroup)
	for _, entry := range entries {
		sessionID := metadataValue(entry.Metadata, "x-session-id")
		model := modelName(entry)
		key := sessionID
		if !joinSessions || sessionID == "" {
			key += "\x00" + model
		}
		group := groups[key]
		if group == nil {
			group = &sessionModelGroup{SessionID: sessionID, Models: make(map[string]struct{}), UserAgents: make(map[string]struct{}), Metadata: make(map[string]map[string]struct{}), FirstSeen: entry.CreatedAt, LastSeen: entry.CreatedAt}
			groups[key] = group
		}
		group.Models[model] = struct{}{}
		if entry.UserAgent != "" {
			group.UserAgents[entry.UserAgent] = struct{}{}
		}
		for key, value := range entry.Metadata {
			if value == "" {
				continue
			}
			if group.Metadata[key] == nil {
				group.Metadata[key] = make(map[string]struct{})
			}
			group.Metadata[key][value] = struct{}{}
		}
		if entry.CreatedAt.Before(group.FirstSeen) {
			group.FirstSeen = entry.CreatedAt
		}
		if entry.CreatedAt.After(group.LastSeen) {
			group.LastSeen = entry.CreatedAt
		}
		group.Totals.Requests++
		group.Totals.Duration += entry.Duration
		group.Totals.TokensIn += entry.TokensIn
		group.Totals.TokensOut += entry.TokensOut
		group.Totals.Cost += entry.Cost
	}
	result := make([]sessionModelGroup, 0, len(groups))
	for _, group := range groups {
		group.Model = joinSet(group.Models)
		result = append(result, *group)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].SessionID != result[j].SessionID {
			return result[i].SessionID < result[j].SessionID
		}
		return result[i].Model < result[j].Model
	})
	return result
}

func hasSessionIDs(entries []LogEntry) bool {
	for _, entry := range entries {
		if metadataValue(entry.Metadata, "x-session-id") != "" {
			return true
		}
	}
	return false
}

func joinSet(values map[string]struct{}) string {
	items := make([]string, 0, len(values))
	for value := range values {
		items = append(items, value)
	}
	sort.Strings(items)
	return strings.Join(items, ", ")
}

func cell(s string) string {
	var out strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' {
			out.WriteByte(' ')
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if strings.ContainsRune("\\|*_`[]<>#~!+=", r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

func sum(entries []LogEntry) totals {
	var t totals
	sessions := make(map[string]struct{})
	for _, e := range entries {
		t.Requests++
		sessions[metadataValue(e.Metadata, "x-session-id")] = struct{}{}
		t.TokensIn += e.TokensIn
		t.TokensOut += e.TokensOut
		t.Cost += e.Cost
	}
	t.Sessions = len(sessions)
	return t
}

func group(entries []LogEntry, key func(LogEntry) string) map[string]totals {
	r := make(map[string]totals)
	sessions := make(map[string]map[string]struct{})
	for _, e := range entries {
		groupKey := key(e)
		t := r[groupKey]
		t.Requests++
		if sessions[groupKey] == nil {
			sessions[groupKey] = make(map[string]struct{})
		}
		sessionID := metadataValue(e.Metadata, "x-session-id")
		if _, seen := sessions[groupKey][sessionID]; !seen {
			sessions[groupKey][sessionID] = struct{}{}
			t.Sessions++
		}
		t.TokensIn += e.TokensIn
		t.TokensOut += e.TokensOut
		t.Cost += e.Cost
		r[groupKey] = t
	}
	return r
}

func filterEntriesForSession(entries []LogEntry, sessionID string) []LogEntry {
	filtered := make([]LogEntry, 0, len(entries))
	for _, entry := range entries {
		if metadataValue(entry.Metadata, "x-session-id") == sessionID {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func filterEntriesByTime(entries []LogEntry, start, end time.Time) []LogEntry {
	filtered := make([]LogEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.CreatedAt.Before(start) && entry.CreatedAt.Before(end) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func filterModel(entries []LogEntry, model string) []LogEntry {
	var r []LogEntry
	for _, e := range entries {
		if modelName(e) == model {
			r = append(r, e)
		}
	}
	return r
}

func writeTotals(out *bytes.Buffer, values map[string]totals, tokens bool) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		t := values[key]
		if tokens {
			fmt.Fprintf(out, "| %s | %d | %d | %d | %d | %.6f |\n", cell(key), t.Requests, t.Sessions, t.TokensIn, t.TokensOut, t.Cost)
		} else {
			fmt.Fprintf(out, "| %s | %d | %d | %.6f |\n", cell(key), t.Requests, t.Sessions, t.Cost)
		}
	}
}
