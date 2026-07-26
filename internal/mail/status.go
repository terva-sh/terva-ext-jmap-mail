package mail

import (
	"context"
	"sort"

	"terva-ext-jmap-mail/internal/jmap"
)

// AccountInfo is the email_list_accounts / email_status account shape.
type AccountInfo struct {
	ID                       string `json:"id"`
	Name                     string `json:"name"`
	IsPersonal               bool   `json:"isPersonal"`
	IsReadOnly               bool   `json:"isReadOnly"`
	SupportsMail             bool   `json:"supportsMail"`
	SupportsSubmission       bool   `json:"supportsSubmission"`
	SupportsVacationResponse bool   `json:"supportsVacationResponse"`
	IsPrimaryMail            bool   `json:"isPrimaryMail,omitempty"`
	// CapabilityUrns lists every accountCapabilities key verbatim, so
	// provider-specific and newer-standard capabilities (vendor URLs,
	// urn:ietf:params:jmap:sieve, …) are discoverable without a code change.
	CapabilityUrns []string `json:"capabilityUrns,omitempty"`
}

// Status is the email_status output: configuration + provider shape, no mail
// content and no secrets.
type Status struct {
	Configured   bool             `json:"configured"`
	Hint         string           `json:"hint,omitempty"`
	SessionURL   string           `json:"sessionUrl,omitempty"`
	Username     string           `json:"username,omitempty"`
	AccountID    string           `json:"accountId,omitempty"`
	AccountName  string           `json:"accountName,omitempty"`
	Accounts     []AccountInfo    `json:"accounts,omitempty"`
	Capabilities []string         `json:"capabilities,omitempty"`
	Limits       *jmap.CoreLimits `json:"limits,omitempty"`
	MaxBodyBytes int              `json:"maxBodyBytes,omitempty"`
	// Tool gating: which config knobs are set, which tools they keep
	// unavailable, and how the user can change that. AccessLevel and
	// EnableSieveTools come from config here; the glue layer fills the
	// tool-name list and hint (it owns the tool surface).
	AccessLevel      string   `json:"accessLevel,omitempty"`
	EnableSieveTools bool     `json:"enableSieveTools"`
	UnavailableTools []string `json:"unavailableTools,omitempty"`
	ToolsHint        string   `json:"toolsHint,omitempty"`
	// Audit reports whether this session's activity is being recorded, and
	// where. An audit trail nobody can confirm is running is worth very
	// little: the operator who switched it on needs to see that it took, and
	// the model should know its actions are on the record. The glue layer
	// fills this — internal/mail does not own the log.
	Audit *AuditStatus `json:"audit,omitempty"`
}

// AuditStatus is the email_status view of the audit log.
type AuditStatus struct {
	Enabled bool `json:"enabled"`
	// Path is the file currently being appended to; empty before the first
	// record of the process, which is not an error.
	Path       string `json:"path,omitempty"`
	RetainDays int    `json:"retainDays,omitempty"`
	// Note states the content policy plainly, because the one question an
	// operator asks about a mail audit log is what it keeps.
	Note string `json:"note,omitempty"`
}

// Accounts lists the accounts reachable with the configured credentials.
func (s *Service) Accounts(ctx context.Context) ([]AccountInfo, error) {
	sess, err := s.getSession(ctx)
	if err != nil {
		return nil, err
	}
	return accountInfos(sess), nil
}

func accountInfos(sess *jmap.Session) []AccountInfo {
	primaryMail := sess.PrimaryAccounts[jmap.CapMail]
	var out []AccountInfo
	for id, acct := range sess.Accounts {
		var urns []string
		for urn := range acct.AccountCapabilities {
			urns = append(urns, urn)
		}
		sort.Strings(urns)
		out = append(out, AccountInfo{
			ID:                       id,
			Name:                     acct.Name,
			IsPersonal:               acct.IsPersonal,
			IsReadOnly:               acct.IsReadOnly,
			SupportsMail:             acct.HasCapability(jmap.CapMail),
			SupportsSubmission:       acct.HasCapability(jmap.CapSubmission),
			SupportsVacationResponse: acct.HasCapability(jmap.CapVacationResponse),
			IsPrimaryMail:            id == primaryMail,
			CapabilityUrns:           urns,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Status reports the provider connection: chosen account, capabilities, and
// core limits.
func (s *Service) Status(ctx context.Context) (*Status, error) {
	accountID, sess, err := s.account(ctx, "")
	if err != nil {
		// Session reachable but account selection failed → still report shape.
		sess2, err2 := s.getSession(ctx)
		if err2 != nil {
			return nil, err
		}
		st := s.statusFromSession(sess2)
		st.Hint = err.Error()
		return st, nil
	}
	st := s.statusFromSession(sess)
	st.AccountID = accountID
	st.AccountName = sess.Accounts[accountID].Name
	return st, nil
}

func (s *Service) statusFromSession(sess *jmap.Session) *Status {
	var caps []string
	for urn := range sess.Capabilities {
		caps = append(caps, urn)
	}
	sort.Strings(caps)
	st := &Status{
		Configured:       true,
		SessionURL:       s.cfg.SessionURL,
		Username:         sess.Username,
		Accounts:         accountInfos(sess),
		Capabilities:     caps,
		MaxBodyBytes:     s.cfg.MaxBodyBytes,
		AccessLevel:      s.cfg.AccessLevel,
		EnableSieveTools: s.cfg.EnableSieveTools,
	}
	if limits, ok := sess.CoreLimits(); ok {
		st.Limits = &limits
	}
	return st
}
