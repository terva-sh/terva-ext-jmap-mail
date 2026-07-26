package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"terva-ext-jmap-mail/internal/config"
	"terva-ext-jmap-mail/internal/jmap"
	"terva-ext-jmap-mail/internal/mail"
	"terva-ext-jmap-mail/internal/sievestore"

	"terva.sh/terva/packages/agent/ext"
)

// toolTimeout bounds one tool call's provider round-trips, safely inside the
// host's 60s tool deadline.
const toolTimeout = 45 * time.Second

const notConfiguredMsg = "jmap-mail is not configured — open /extensions, select jmap-mail, " +
	"press c, and set the JMAP session URL and API token (Fastmail: Settings → Privacy & Security → Manage API tokens)."

// app is the thin SDK glue: config snapshot → mail.Service lifecycle, tool
// arg parsing, JSON result rendering, and tool visibility.
type app struct {
	e *ext.Extension

	mu       sync.Mutex
	settings config.Settings
	svc      *mail.Service
	sieve    *sievestore.Store
	// sessionCWD is the active session's working directory (refreshed on
	// every session_start / /cd). It is the jail root for sourcePath file
	// imports — the host jails its own read tool to the workspace, and the
	// extension must not be a way around that.
	sessionCWD string
}

func newApp(e *ext.Extension) *app { return &app{e: e} }

func (a *app) currentSettings() config.Settings {
	c := a.e.Config()
	return config.Normalize(config.Settings{
		SessionURL:       c.String("session_url"),
		APIToken:         c.String("api_token"),
		DefaultAccount:   c.String("default_account"),
		MaxBodyBytes:     c.Int("max_body_bytes"),
		AccessLevel:      c.String("access_level"),
		EnableSieveTools: c.Bool("enable_sieve_tools"),
	})
}

// service returns a mail.Service for the current config, rebuilding when the
// config changed (which drops all caches with it).
func (a *app) service() (*mail.Service, error) {
	st := a.currentSettings()
	if !st.Configured() {
		return nil, errNotConfigured{}
	}
	if err := st.Validate(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.svc == nil || a.settings != st {
		a.svc = mail.NewService(jmap.NewClient(st.SessionURL, st.APIToken), st)
		a.settings = st
	}
	return a.svc, nil
}

type errNotConfigured struct{}

func (errNotConfigured) Error() string { return notConfiguredMsg }

// sieveStore returns the shared local document store, rooted in the
// extension's writable data dir. The sieve tools never touch the network —
// that's what keeps their local-read/local-data declarations honest.
func (a *app) sieveStore() (*sievestore.Store, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sieve != nil {
		return a.sieve, nil
	}
	dataDir := a.e.Host().DataDir
	if dataDir == "" {
		return nil, fmt.Errorf("host provided no writable data directory for extension state")
	}
	a.sieve = sievestore.New(filepath.Join(dataDir, "sieve"))
	return a.sieve, nil
}

// sieveKey builds the document key without any network call: provider host
// from config, account from the explicit arg or — when the store already has
// exactly one account for this provider — that one. First use needs an
// explicit accountId (email_status shows the canonical id).
func (a *app) sieveKey(store *sievestore.Store, accountArg, name string) (sievestore.DocKey, error) {
	st := a.currentSettings()
	if !st.Configured() {
		return sievestore.DocKey{}, errNotConfigured{}
	}
	u, err := url.Parse(st.SessionURL)
	if err != nil || u.Hostname() == "" {
		return sievestore.DocKey{}, fmt.Errorf("cannot derive provider from session_url")
	}
	// The store key must satisfy the DocKey charset: hostname only (a port's
	// ":" would fail validation and break every sieve tool on self-hosted
	// servers), with IPv6 colons mapped. Same host on different ports shares
	// a store — same provider for our purposes.
	provider := strings.ReplaceAll(u.Hostname(), ":", "-")
	account := strings.TrimSpace(accountArg)
	if account == "" {
		accounts, err := store.Accounts(provider)
		if err != nil {
			return sievestore.DocKey{}, err
		}
		switch len(accounts) {
		case 1:
			account = accounts[0]
		case 0:
			return sievestore.DocKey{}, fmt.Errorf("no sieve documents stored yet for %s — pass accountId explicitly (email_status shows the canonical account id)", provider)
		default:
			return sievestore.DocKey{}, fmt.Errorf("multiple accounts have sieve documents under %s (%s) — pass accountId", provider, strings.Join(accounts, ", "))
		}
	}
	return sievestore.DocKey{Provider: provider, Account: account, Name: name}, nil
}

func toolCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), toolTimeout)
}

// jsonResult renders a tool result as indented JSON (small payloads; the
// bounds live in internal/mail).
func jsonResult(v any) ext.ToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ext.TextErrorResult("encode result: " + err.Error())
	}
	return ext.TextResult(string(b))
}

// --- lifecycle ---

// onConfig logs the change's shape (never values) and re-syncs visibility.
// Off-boundary tool changes are safe (they apply next turn) and re-asserting
// an unchanged set is a host-side no-op.
func (a *app) onConfig(_ ext.Config) {
	st := a.currentSettings()
	a.e.Logf("config updated: %s", st.Redacted())
	a.syncVisibility(nil)
}

// onSession is the cache-safe boundary: assert tool visibility for this
// session (withdraw everything while unconfigured — the tools could only
// refuse; restore once configured), and track the session CWD for the
// sourcePath import jail.
func (a *app) onSession(s ext.Session) {
	a.mu.Lock()
	a.sessionCWD = s.CWD
	a.mu.Unlock()
	a.syncVisibility(&s)
}

// toolVisibility is satisfied by both ext.Session (the cache-safe boundary
// handle) and *ext.Extension (the anywhere escape hatch used on config_update).
type toolVisibility interface {
	WithdrawAllTools()
	RestoreAllTools()
	WithdrawTools(names ...string)
}

// sieveToolNames are withdrawn together when the sieve store is disabled.
var sieveToolNames = []string{
	"email_sieve_list", "email_sieve_get", "email_sieve_diff",
	"email_sieve_put", "email_sieve_restore", "email_sieve_mark_applied",
	"email_sieve_archive",
}

func (a *app) syncVisibility(s *ext.Session) {
	if a.e.Host().ProtocolVersion < 4 {
		return // host can't hide tools; handlers return clear errors instead
	}
	var v toolVisibility = a.e
	if s != nil {
		v = *s
	}
	st := a.currentSettings()
	if !st.Configured() {
		v.WithdrawAllTools() // every tool could only refuse
		return
	}
	// Reset the withdrawn set, then hide what config keeps off. The SDK
	// sends the final snapshot; re-asserting unchanged is a host no-op.
	v.RestoreAllTools()
	if withdraw := gatedTools(st); len(withdraw) > 0 {
		v.WithdrawTools(withdraw...)
	}
}

// gatedTools lists the tools the given settings keep off — the withdraw set
// for syncVisibility, and email_status's explanation of what is absent.
func gatedTools(st config.Settings) []string {
	var withdraw []string
	if !st.AllowOrganize() {
		withdraw = append(withdraw, "email_mark", "email_move", "email_trash")
	}
	if !st.AllowDestroy() {
		withdraw = append(withdraw, "email_destroy")
	}
	if !st.EnableSieveTools {
		withdraw = append(withdraw, sieveToolNames...)
	}
	return withdraw
}

// requireOrganize is the handler-level guard behind the withdrawn tools
// (defense in depth, and the whole story on pre-protocol-4 hosts).
func (a *app) requireOrganize() error {
	if !a.currentSettings().AllowOrganize() {
		return fmt.Errorf("organization tools are disabled at access_level=read-only: the user can raise it to read-organize for jmap-mail in /extensions (press c)")
	}
	return nil
}

// requireSieve gates the sieve store tools on their opt-in flag.
func (a *app) requireSieve() error {
	if !a.currentSettings().EnableSieveTools {
		return fmt.Errorf("sieve tools are disabled: the user can turn on enable_sieve_tools for jmap-mail in /extensions (press c)")
	}
	return nil
}

// --- tool handlers ---

func (a *app) handleStatus(_ json.RawMessage) ext.ToolResult {
	st := a.currentSettings()
	if !st.Configured() {
		return jsonResult(mail.Status{Configured: false, Hint: notConfiguredMsg})
	}
	svc, err := a.service()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	ctx, cancel := toolCtx()
	defer cancel()
	status, err := svc.Status(ctx)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	if gated := gatedTools(st); len(gated) > 0 {
		status.UnavailableTools = gated
		status.ToolsHint = "these tools are off by configuration (access_level / enable_sieve_tools); only the user can enable them, in /extensions (press c on jmap-mail)"
	}
	return jsonResult(status)
}

func (a *app) handleListAccounts(_ json.RawMessage) ext.ToolResult {
	svc, err := a.service()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	ctx, cancel := toolCtx()
	defer cancel()
	accounts, err := svc.Accounts(ctx)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(map[string]any{"accounts": accounts})
}

func (a *app) handleListMailboxes(raw json.RawMessage) ext.ToolResult {
	var in struct {
		AccountID     string   `json:"accountId"`
		IncludeCounts *bool    `json:"includeCounts"`
		Mailboxes     []string `json:"mailboxes"`
		Fields        []string `json:"fields"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	svc, err := a.service()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	includeCounts := in.IncludeCounts == nil || *in.IncludeCounts
	ctx, cancel := toolCtx()
	defer cancel()
	list, err := svc.ListMailboxes(ctx, mail.ListMailboxesParams{
		Account: in.AccountID, IncludeCounts: includeCounts,
		Mailboxes: in.Mailboxes, Fields: in.Fields,
	})
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(list)
}

func (a *app) handleSearch(raw json.RawMessage) ext.ToolResult {
	var in struct {
		AccountID       string          `json:"accountId"`
		Mailbox         string          `json:"mailbox"`
		Text            string          `json:"text"`
		From            string          `json:"from"`
		To              string          `json:"to"`
		Cc              string          `json:"cc"`
		Bcc             string          `json:"bcc"`
		Subject         string          `json:"subject"`
		Body            string          `json:"body"`
		After           string          `json:"after"`
		Before          string          `json:"before"`
		HasAttachment   json.RawMessage `json:"hasAttachment"`
		Keyword         string          `json:"keyword"`
		NotKeyword      string          `json:"notKeyword"`
		FilterJSON      json.RawMessage `json:"filterJson"`
		CollapseThreads bool            `json:"collapseThreads"`
		IncludeTotal    bool            `json:"includeTotal"`
		Fields          []string        `json:"fields"`
		ReturnIDs       string          `json:"returnIds"`
		Limit           int             `json:"limit"`
		Position        int             `json:"position"`
		Sort            string          `json:"sort"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	hasAttachment, err := parseHasAttachment(in.HasAttachment)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	svc, err := a.service()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	ctx, cancel := toolCtx()
	defer cancel()
	result, err := svc.Search(ctx, mail.SearchParams{
		Account: in.AccountID, Mailbox: in.Mailbox,
		Text: in.Text, From: in.From, To: in.To, Cc: in.Cc, Bcc: in.Bcc,
		Subject: in.Subject, Body: in.Body, After: in.After, Before: in.Before,
		HasAttachment: hasAttachment, Keyword: in.Keyword, NotKeyword: in.NotKeyword,
		FilterJSON: in.FilterJSON, CollapseThreads: in.CollapseThreads, IncludeTotal: in.IncludeTotal,
		Fields: in.Fields, ReturnIDs: in.ReturnIDs,
		Limit: in.Limit, Position: in.Position, Sort: in.Sort,
	})
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(result)
}

// parseHasAttachment reads the tri-state attachment filter. It is declared as
// a string enum ("" | yes | no) rather than a boolean because a model that
// pads every declared property would send false — an active filter excluding
// every message that has an attachment, applied silently with nothing in the
// result to notice it by. "" is the inert value the schema can express.
// Booleans are still accepted so a caller working from the older schema, or
// from habit, is not broken.
func parseHasAttachment(raw json.RawMessage) (*bool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		// A JSON null unmarshals into a string as a no-op, so it lands here
		// as "" — the same "no filter" the padded value means.
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "", "any":
			return nil, nil
		case "yes", "true":
			return boolPtr(true), nil
		case "no", "false":
			return boolPtr(false), nil
		default:
			return nil, fmt.Errorf("hasAttachment %q: use \"yes\" for only messages with an attachment, \"no\" for only messages without, or \"\" to not filter on it", s)
		}
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return &b, nil
	}
	return nil, fmt.Errorf("hasAttachment: use \"yes\", \"no\", or \"\" to not filter on it")
}

func boolPtr(b bool) *bool { return &b }

func (a *app) handleGet(raw json.RawMessage) ext.ToolResult {
	var in struct {
		AccountID       string   `json:"accountId"`
		IDs             []string `json:"ids"`
		Fields          []string `json:"fields"`
		BodyFormat      string   `json:"bodyFormat"`
		MaxBodyBytes    int      `json:"maxBodyBytes"`
		IncludeFullUrls bool     `json:"includeFullUrls"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	svc, err := a.service()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	ctx, cancel := toolCtx()
	defer cancel()
	result, err := svc.Get(ctx, mail.GetParams{
		Account: in.AccountID, IDs: in.IDs, Fields: in.Fields, BodyFormat: in.BodyFormat,
		MaxBodyBytes: in.MaxBodyBytes, IncludeFullUrls: in.IncludeFullUrls,
	})
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(result)
}

func (a *app) handleMark(raw json.RawMessage) ext.ToolResult {
	if err := a.requireOrganize(); err != nil {
		return ext.TextErrorResult(err.Error())
	}
	var in struct {
		AccountID       string   `json:"accountId"`
		IDs             []string `json:"ids"`
		Selection       string   `json:"selection"`
		SelectionOffset int      `json:"selectionOffset"`
		Receipt         string   `json:"receipt"`
		Action          string   `json:"action"`
		DryRun          bool     `json:"dryRun"`
		Confirm         string   `json:"confirm"`
		Verbose         *bool    `json:"verbose"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	svc, err := a.service()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	ctx, cancel := toolCtx()
	defer cancel()
	result, err := svc.Mark(ctx, mail.MarkParams{
		Account: in.AccountID, IDs: in.IDs, Action: in.Action, DryRun: in.DryRun, Confirm: in.Confirm,
		Selection: in.Selection, SelectionOffset: in.SelectionOffset, Receipt: in.Receipt,
		Verbose: in.Verbose,
	})
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(result)
}

func (a *app) handleMove(raw json.RawMessage) ext.ToolResult {
	if err := a.requireOrganize(); err != nil {
		return ext.TextErrorResult(err.Error())
	}
	var in struct {
		AccountID       string   `json:"accountId"`
		IDs             []string `json:"ids"`
		Selection       string   `json:"selection"`
		SelectionOffset int      `json:"selectionOffset"`
		Receipt         string   `json:"receipt"`
		ToMailbox       string   `json:"toMailbox"`
		KeepInMailboxes bool     `json:"keepInMailboxes"`
		DryRun          bool     `json:"dryRun"`
		Confirm         string   `json:"confirm"`
		Verbose         *bool    `json:"verbose"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	svc, err := a.service()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	ctx, cancel := toolCtx()
	defer cancel()
	result, err := svc.Move(ctx, mail.MoveParams{
		Account: in.AccountID, IDs: in.IDs, ToMailbox: in.ToMailbox,
		Selection: in.Selection, SelectionOffset: in.SelectionOffset, Receipt: in.Receipt,
		KeepInMailboxes: in.KeepInMailboxes, DryRun: in.DryRun, Confirm: in.Confirm,
		Verbose: in.Verbose,
	})
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(result)
}

func (a *app) handleTrash(raw json.RawMessage) ext.ToolResult {
	if err := a.requireOrganize(); err != nil {
		return ext.TextErrorResult(err.Error())
	}
	var in struct {
		AccountID       string   `json:"accountId"`
		IDs             []string `json:"ids"`
		Selection       string   `json:"selection"`
		SelectionOffset int      `json:"selectionOffset"`
		Receipt         string   `json:"receipt"`
		DryRun          bool     `json:"dryRun"`
		Confirm         string   `json:"confirm"`
		Verbose         *bool    `json:"verbose"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	svc, err := a.service()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	ctx, cancel := toolCtx()
	defer cancel()
	result, err := svc.Trash(ctx, mail.TrashParams{
		Account: in.AccountID, IDs: in.IDs, DryRun: in.DryRun, Confirm: in.Confirm,
		Selection: in.Selection, SelectionOffset: in.SelectionOffset, Receipt: in.Receipt,
		Verbose: in.Verbose,
	})
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(result)
}

func (a *app) handleDestroy(raw json.RawMessage) ext.ToolResult {
	// Config gate first: below access_level=read-organize-destroy this tool
	// is withdrawn on protocol ≥ 4 hosts, and refuses everywhere else.
	if !a.currentSettings().AllowDestroy() {
		return ext.TextErrorResult("email_destroy is disabled: the user must set access_level to read-organize-destroy for jmap-mail in /extensions (press c). " +
			"email_trash (recoverable) is the alternative when organization is enabled.")
	}
	var in struct {
		AccountID       string   `json:"accountId"`
		IDs             []string `json:"ids"`
		AllowNotInTrash bool     `json:"allowNotInTrash"`
		DryRun          bool     `json:"dryRun"`
		Confirm         string   `json:"confirm"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	svc, err := a.service()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	ctx, cancel := toolCtx()
	defer cancel()
	result, err := svc.Destroy(ctx, mail.DestroyParams{
		Account: in.AccountID, IDs: in.IDs, AllowNotInTrash: in.AllowNotInTrash,
		DryRun: in.DryRun, Confirm: in.Confirm,
	})
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(result)
}

// --- sieve store handlers (local-only; see docs/sieve-workspace-design.md) ---

func (a *app) handleSieveList(raw json.RawMessage) ext.ToolResult {
	if err := a.requireSieve(); err != nil {
		return ext.TextErrorResult(err.Error())
	}
	var in struct {
		IncludeArchived bool `json:"includeArchived"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	store, err := a.sieveStore()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	docs, err := store.List(in.IncludeArchived)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	if docs == nil {
		docs = []sievestore.DocInfo{} // an empty store is [], not null
	}
	out := map[string]any{"documents": docs}
	if len(docs) == 0 {
		out["hint"] = "empty store — import sections with email_sieve_put (origin: paste-in); see the sieve-rules skill"
	}
	return jsonResult(out)
}

func (a *app) handleSieveGet(raw json.RawMessage) ext.ToolResult {
	if err := a.requireSieve(); err != nil {
		return ext.TextErrorResult(err.Error())
	}
	var in struct {
		AccountID string `json:"accountId"`
		Name      string `json:"name"`
		Version   int    `json:"version"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	store, err := a.sieveStore()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	key, err := a.sieveKey(store, in.AccountID, in.Name)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	content, meta, err := store.Get(key, in.Version)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(map[string]any{"key": key, "meta": meta, "content": content})
}

func (a *app) handleSieveDiff(raw json.RawMessage) ext.ToolResult {
	if err := a.requireSieve(); err != nil {
		return ext.TextErrorResult(err.Error())
	}
	var in struct {
		AccountID string `json:"accountId"`
		Name      string `json:"name"`
		From      int    `json:"from"`
		To        int    `json:"to"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	store, err := a.sieveStore()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	key, err := a.sieveKey(store, in.AccountID, in.Name)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	info, err := store.Info(key)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	from, to := in.From, in.To
	if to == 0 {
		to = info.Head
	}
	if from == 0 {
		if info.Applied == 0 {
			return ext.TextErrorResult("no applied version recorded for " + key.Name + " — pass from explicitly, or email_sieve_mark_applied first")
		}
		from = info.Applied
	}
	diff, err := store.Diff(key, from, to)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	out := map[string]any{"key": key, "from": from, "to": to}
	if diff == "" {
		out["identical"] = true
	} else {
		out["diff"] = diff
	}
	return jsonResult(out)
}

func (a *app) handleSievePut(raw json.RawMessage) ext.ToolResult {
	if err := a.requireSieve(); err != nil {
		return ext.TextErrorResult(err.Error())
	}
	var in struct {
		AccountID   string `json:"accountId"`
		Name        string `json:"name"`
		Content     string `json:"content"`
		SourcePath  string `json:"sourcePath"`
		Origin      string `json:"origin"`
		Note        string `json:"note"`
		ContextOnly *bool  `json:"contextOnly"`
		DryRun      bool   `json:"dryRun"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	content, note := in.Content, in.Note
	if in.SourcePath != "" {
		if in.Content != "" {
			return ext.TextErrorResult("pass content OR sourcePath, not both")
		}
		fileContent, err := a.readSieveSource(in.SourcePath)
		if err != nil {
			return ext.TextErrorResult(err.Error())
		}
		content = fileContent
		if note == "" {
			note = "imported from " + in.SourcePath
		} else {
			note += " (source: " + in.SourcePath + ")"
		}
	}
	store, err := a.sieveStore()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	key, err := a.sieveKey(store, in.AccountID, in.Name)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}

	lint := sievestore.Lint(content)
	if sievestore.SuspectsTruncation(note) {
		lint.Findings = append(lint.Findings, sievestore.Finding{
			Level:   "warning",
			Message: "the note mentions truncation — mark_applied will refuse this version without force:true",
		})
	}
	if lint.Findings == nil {
		lint.Findings = []sievestore.Finding{}
	}

	if in.DryRun {
		out := map[string]any{"dryRun": true, "key": key, "lint": lint}
		current, _, err := store.Get(key, 0)
		switch {
		case err == nil:
			if diff := sievestore.Unified(key.Name+"@head", key.Name+"@proposed", current, content); diff == "" {
				out["identical"] = true
			} else {
				out["diff"] = diff
			}
		default:
			out["newDocument"] = true
		}
		return jsonResult(out)
	}

	meta, appended, err := store.Put(key, content, in.Origin, note)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	out := map[string]any{"key": key, "appended": appended, "meta": meta, "lint": lint}
	if in.ContextOnly != nil {
		if err := store.SetContextOnly(key, *in.ContextOnly); err != nil {
			return ext.TextErrorResult(err.Error())
		}
		out["contextOnly"] = *in.ContextOnly
	}
	return jsonResult(out)
}

// readSieveSource imports a file for email_sieve_put, jailed to the active
// session's working directory: the host confines its own file tools to the
// workspace, so this tool refuses to read anywhere the agent can't.
func (a *app) readSieveSource(path string) (string, error) {
	a.mu.Lock()
	cwd := a.sessionCWD
	a.mu.Unlock()
	if cwd == "" {
		return "", fmt.Errorf("sourcePath needs an active session working directory to confine reads — paste the content instead")
	}
	jail, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve session directory: %v", err)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(jail, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("sourcePath: %v", err)
	}
	if resolved != jail && !strings.HasPrefix(resolved, jail+string(filepath.Separator)) {
		return "", fmt.Errorf("sourcePath %q is outside the session working directory %q — move the file into the workspace first", path, cwd)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("sourcePath: %v", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("sourcePath %q is not a regular file", path)
	}
	if fi.Size() > sievestore.MaxContentBytes {
		return "", fmt.Errorf("sourcePath file is %d bytes; the per-version limit is %d", fi.Size(), sievestore.MaxContentBytes)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("sourcePath: %v", err)
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("sourcePath %q is not UTF-8 text", path)
	}
	return string(raw), nil
}

func (a *app) handleSieveRestore(raw json.RawMessage) ext.ToolResult {
	if err := a.requireSieve(); err != nil {
		return ext.TextErrorResult(err.Error())
	}
	var in struct {
		AccountID string `json:"accountId"`
		Name      string `json:"name"`
		Version   int    `json:"version"`
		Note      string `json:"note"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	store, err := a.sieveStore()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	key, err := a.sieveKey(store, in.AccountID, in.Name)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	meta, err := store.Restore(key, in.Version, in.Note)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(map[string]any{"key": key, "meta": meta})
}

func (a *app) handleSieveMarkApplied(raw json.RawMessage) ext.ToolResult {
	if err := a.requireSieve(); err != nil {
		return ext.TextErrorResult(err.Error())
	}
	var in struct {
		AccountID string `json:"accountId"`
		Name      string `json:"name"`
		Version   int    `json:"version"`
		Force     bool   `json:"force"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	store, err := a.sieveStore()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	key, err := a.sieveKey(store, in.AccountID, in.Name)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	meta, err := store.MarkApplied(key, in.Version, in.Force)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(map[string]any{"key": key, "applied": meta.Version, "meta": meta})
}

func (a *app) handleSieveArchive(raw json.RawMessage) ext.ToolResult {
	if err := a.requireSieve(); err != nil {
		return ext.TextErrorResult(err.Error())
	}
	var in struct {
		AccountID string `json:"accountId"`
		Name      string `json:"name"`
		Unarchive bool   `json:"unarchive"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	store, err := a.sieveStore()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	key, err := a.sieveKey(store, in.AccountID, in.Name)
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	if in.Unarchive {
		if err := store.Unarchive(key); err != nil {
			return ext.TextErrorResult(err.Error())
		}
		return jsonResult(map[string]any{"key": key, "archived": false})
	}
	if err := store.Archive(key); err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(map[string]any{"key": key, "archived": true,
		"note": "hidden from email_sieve_list unless includeArchived; every version kept on disk"})
}

func (a *app) handleGetThread(raw json.RawMessage) ext.ToolResult {
	var in struct {
		AccountID       string   `json:"accountId"`
		ThreadID        string   `json:"threadId"`
		EmailID         string   `json:"emailId"`
		IncludeBodies   bool     `json:"includeBodies"`
		Fields          []string `json:"fields"`
		Limit           int      `json:"limit"`
		MaxBodyBytes    int      `json:"maxBodyBytes"`
		IncludeFullUrls bool     `json:"includeFullUrls"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ext.TextErrorResult("invalid args: " + err.Error())
	}
	svc, err := a.service()
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	ctx, cancel := toolCtx()
	defer cancel()
	result, err := svc.GetThread(ctx, mail.ThreadParams{
		Account: in.AccountID, ThreadID: in.ThreadID, EmailID: in.EmailID,
		IncludeBodies: in.IncludeBodies, Fields: in.Fields, Limit: in.Limit,
		MaxBodyBytes: in.MaxBodyBytes, IncludeFullUrls: in.IncludeFullUrls,
	})
	if err != nil {
		return ext.TextErrorResult(err.Error())
	}
	return jsonResult(result)
}
