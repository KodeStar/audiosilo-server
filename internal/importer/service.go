package importer

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// User is an AudioSilo account as the importer needs it.
type User struct {
	ID       int64
	Username string
	Admin    bool
}

// Users looks up AudioSilo accounts (auth's, which this package doesn't import).
// User returns ErrUnknownUser for an id with no account.
type Users interface {
	ListUsers(ctx context.Context) ([]User, error)
	User(ctx context.Context, id int64) (User, error)
}

// ErrUnknownUser is a user id with no account.
var ErrUnknownUser = errors.New("unknown user")

// InvalidError is a start request the importer refuses; its message is safe to
// show.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// fetchTimeout bounds one background fetch (every user of one start).
const fetchTimeout = 30 * time.Minute

// Service runs imports: the ABS connection check, the background fetch, the
// review (a cutoff change re-plans from the stored payload), the apply and the
// undo. The ABS token is held only by the running fetch, never stored or logged.
type Service struct {
	cat       *catalog.Catalog
	users     Users
	loc       *time.Location
	retention func() time.Duration
	log       *slog.Logger
	now       func() time.Time
	wg        sync.WaitGroup
}

// New returns a Service. loc is the server's zone (the days listening is counted
// in) and retention how long raw sessions are kept (read at each apply: a
// setting), both as PruneSessions is given them: an apply writes the sessions
// already past the retention straight into day totals. A nil retention writes
// every session raw.
func New(cat *catalog.Catalog, users Users, loc *time.Location, retention func() time.Duration, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if loc == nil {
		loc = time.Local
	}
	return &Service{cat: cat, users: users, loc: loc, retention: retention, log: log, now: time.Now}
}

// Wait blocks until no fetch is running (tests, shutdown).
func (s *Service) Wait() { s.wg.Wait() }

// AbsUser is an ABS user the admin can map onto an AudioSilo one.
type AbsUser struct {
	ABSID    string `json:"abs_id"`
	Username string `json:"username"`
	Type     string `json:"type"`
	// SuggestedUserID is the AudioSilo user with the same username (any case).
	SuggestedUserID *int64 `json:"suggested_user_id"`
}

// maxToken bounds an ABS token (theirs are JWTs of a few hundred bytes).
const maxToken = 8192

// checkToken is an ABS token as the client sends it (trimmed), or an
// *InvalidError: empty, too long, or with a character that can't go in a header.
func checkToken(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	switch {
	case token == "":
		return "", &InvalidError{"the API token is required"}
	case len(token) > maxToken || strings.ContainsAny(token, "\r\n\x00"):
		return "", &InvalidError{"the API token is not valid"}
	}
	return token, nil
}

// Connect checks the ABS server at rawURL with token and lists its users (just
// the token's own with a non-admin token), each with the AudioSilo user of the
// same name. ErrInvalidURL, *InvalidError (the token), or an *Error.
func (s *Service) Connect(ctx context.Context, rawURL, token string) (string, []AbsUser, error) {
	base, err := ParseBaseURL(rawURL)
	if err != nil {
		return "", nil, err
	}
	if token, err = checkToken(token); err != nil {
		return "", nil, err
	}
	c := NewClient(base, token)
	version, err := c.Status(ctx)
	if err != nil {
		return "", nil, err
	}
	absUsers, err := c.Users(ctx)
	if err != nil {
		return "", nil, err
	}
	local, err := s.users.ListUsers(ctx)
	if err != nil {
		return "", nil, err
	}
	byName := map[string]int64{}
	for _, u := range local {
		byName[strings.ToLower(u.Username)] = u.ID
	}
	out := make([]AbsUser, 0, len(absUsers))
	for _, u := range absUsers {
		au := AbsUser{ABSID: u.ID, Username: u.Username, Type: u.Type}
		if id, ok := byName[strings.ToLower(u.Username)]; ok {
			au.SuggestedUserID = &id
		}
		out = append(out, au)
	}
	return version, out, nil
}

// Cutoff is a requested cutoff: Auto (the person's first listening here), At,
// or neither (no cutoff: every session is imported).
type Cutoff struct {
	Auto bool
	At   time.Time
}

// resolve is c for userID, as imports.cutoff stores it.
func (s *Service) resolve(ctx context.Context, c Cutoff, userID int64) (*string, error) {
	if !c.Auto {
		return catalog.FormatCutoff(c.At), nil
	}
	first, err := s.cat.ListeningStart(ctx, userID, s.loc)
	if err != nil {
		return nil, err
	}
	return catalog.FormatCutoff(first), nil
}

// Mapping is one ABS user's history going to one AudioSilo user. ABSUsername
// (optional, the users list's) names the source until the fetch reads ABS's own.
type Mapping struct {
	ABSUserID   string
	ABSUsername string
	UserID      int64
}

// StartRequest is an import of one or more ABS users from one server.
type StartRequest struct {
	URL    string
	Token  string
	Users  []Mapping
	Cutoff Cutoff
}

// maxMappings bounds one start.
const maxMappings = 100

// Start records an import per mapping (status fetching) and fetches them in the
// background under base (the server's lifetime), returning them as they start.
// ErrInvalidURL, *InvalidError, or catalog.ErrImportRunning when one of the
// users already has an import fetching or applying.
func (s *Service) Start(ctx, base context.Context, req StartRequest) ([]catalog.Import, error) {
	u, err := ParseBaseURL(req.URL)
	if err != nil {
		return nil, err
	}
	token, err := checkToken(req.Token)
	if err != nil {
		return nil, err
	}
	switch {
	case len(req.Users) == 0:
		return nil, &InvalidError{"choose at least one user to import"}
	case len(req.Users) > maxMappings:
		return nil, &InvalidError{"too many users in one import"}
	}
	seen := map[int64]bool{}
	imps := make([]catalog.Import, 0, len(req.Users))
	for _, m := range req.Users {
		if m.ABSUserID == "" || len(m.ABSUserID) > 200 {
			return nil, &InvalidError{"every mapping needs an Audiobookshelf user"}
		}
		if len(m.ABSUsername) > 200 {
			return nil, &InvalidError{"an Audiobookshelf username is too long"}
		}
		if seen[m.UserID] {
			return nil, &InvalidError{"each AudioSilo user can receive one import at a time"}
		}
		seen[m.UserID] = true
		if _, err := s.users.User(ctx, m.UserID); errors.Is(err, ErrUnknownUser) {
			return nil, &InvalidError{"unknown user"}
		} else if err != nil {
			return nil, err
		}
		cutoff, err := s.resolve(ctx, req.Cutoff, m.UserID)
		if err != nil {
			return nil, err
		}
		imps = append(imps, catalog.Import{UserID: m.UserID, Source: catalog.ImportSourceABS,
			SourceURL: u.String(), SourceUser: strings.TrimSpace(m.ABSUsername), SourceID: m.ABSUserID,
			Cutoff: cutoff})
	}
	created, err := s.cat.CreateImports(ctx, imps)
	if err != nil {
		return nil, err
	}
	c := NewClient(u, token)
	s.wg.Go(func() {
		work, cancel := context.WithTimeout(base, fetchTimeout)
		defer cancel()
		s.fetch(work, c, created)
	})
	return created, nil
}

// fetch reads the ABS history of each import's user and plans it, moving each to
// review (or failed). The server's items and the books here are read and indexed
// once, for every user. A fetch the server's shutdown stopped is left fetching:
// the next start settles it (catalog.InterruptImports).
func (s *Service) fetch(ctx context.Context, c *Client, imps []catalog.Import) {
	fail := func(imp catalog.Import, err error) {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		code, msg := CodeFetchFailed, "The import failed. The server log has the details."
		var ae *Error
		if errors.As(err, &ae) {
			code, msg = ae.Code, ae.Msg
		} else if ctx.Err() != nil {
			code, msg = CodeUnreachable, "Audiobookshelf took too long to answer."
		}
		// Neither the error (a network failure or a decoding one, never a
		// response body) nor anything logged here carries the token.
		s.log.Warn("import: fetch failed", "import", imp.ID, "code", code, "err", err)
		if ferr := s.cat.FailImport(context.WithoutCancel(ctx), imp.ID, code, msg); ferr != nil {
			s.log.Warn("import: record failure failed", "import", imp.ID, "err", ferr)
		}
	}
	failAll := func(err error) {
		for _, imp := range imps {
			fail(imp, err)
		}
	}
	if _, err := c.Status(ctx); err != nil {
		failAll(err)
		return
	}
	items, err := c.items(ctx)
	if err != nil {
		failAll(err)
		return
	}
	books, err := s.bookIndex(ctx)
	if err != nil {
		failAll(err)
		return
	}
	byItem := newItemIndex(items)
	for _, imp := range imps {
		if err := s.fetchOne(ctx, c, imp, byItem, books); err != nil {
			fail(imp, err)
		}
	}
}

// fetchOne fetches and plans one import.
func (s *Service) fetchOne(ctx context.Context, c *Client, imp catalog.Import, items *itemIndex, books *bookIndex) error {
	u, own, err := c.userDetail(ctx, imp.SourceID)
	if err != nil {
		return err
	}
	sessions, err := c.sessions(ctx, imp.SourceID, own)
	if err != nil {
		return err
	}
	p := buildPayload(u, sessions, items)
	st, err := s.cat.ImportStateFor(ctx, imp.ID)
	if err != nil {
		return err
	}
	pl, err := s.review(ctx, st, p, books)
	if err != nil {
		return err
	}
	enc, err := encodePayload(p)
	if err != nil {
		return err
	}
	return s.cat.FinishImportFetch(ctx, imp.ID, u.Username, enc, pl)
}

// bookIndex reads and indexes the books here, for matching.
func (s *Service) bookIndex(ctx context.Context) (*bookIndex, error) {
	books, err := s.cat.ImportBooks(ctx)
	if err != nil {
		return nil, err
	}
	return newBookIndex(books), nil
}

// matchesFor matches p's items against the books in books that userID can
// access.
func (s *Service) matchesFor(ctx context.Context, userID int64, p *Payload, books *bookIndex) (map[string]Match, error) {
	u, err := s.users.User(ctx, userID)
	if err != nil {
		return nil, err
	}
	scopes, err := s.cat.UserScopes(ctx, userID, u.Admin)
	if err != nil {
		return nil, err
	}
	m := books.matcher(func(ref catalog.Ref) bool { return catalog.ScopesAllow(scopes, ref) })
	return m.matchAll(p.Items), nil
}

// review plans an import from p against st, as the review shows it.
func (s *Service) review(ctx context.Context, st *catalog.ImportState, p *Payload, books *bookIndex) (catalog.ImportPlan, error) {
	matches, err := s.matchesFor(ctx, st.Import.UserID, p, books)
	if err != nil {
		return catalog.ImportPlan{}, err
	}
	return plan(p, matches, *st, s.loc, s.now()).Plan, nil
}

// reviewing loads import id, which must be in review: its state (read once) and
// its payload.
func (s *Service) reviewing(ctx context.Context, id int64) (*catalog.ImportState, *Payload, error) {
	st, err := s.cat.ImportStateFor(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if st.Import.Status != catalog.ImportReview {
		return nil, nil, catalog.ErrImportNotReady
	}
	b, err := s.cat.ImportPayload(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	p, err := decodePayload(b)
	if err != nil {
		return nil, nil, err
	}
	return st, p, nil
}

// SetCutoff changes a reviewed import's cutoff and plans it again from the
// stored payload. catalog.ErrNotFound, or ErrImportNotReady unless in review.
func (s *Service) SetCutoff(ctx context.Context, id int64, c Cutoff) (*catalog.ImportDetail, error) {
	st, p, err := s.reviewing(ctx, id)
	if err != nil {
		return nil, err
	}
	if st.Import.Cutoff, err = s.resolve(ctx, c, st.Import.UserID); err != nil {
		return nil, err
	}
	books, err := s.bookIndex(ctx)
	if err != nil {
		return nil, err
	}
	pl, err := s.review(ctx, st, p, books)
	if err != nil {
		return nil, err
	}
	if err := s.cat.SetImportReview(ctx, id, st.Import.Cutoff, pl); err != nil {
		return nil, err
	}
	return s.cat.GetImport(ctx, id)
}

// Apply writes a reviewed import (catalog.ApplyImport: replacing the person's
// previous one from the same source). The books are matched again now, so a
// book added since the review is found. Sessions older than the retention go
// straight into day totals (catalog.ImportWrite.RollUpBefore).
func (s *Service) Apply(ctx context.Context, id int64) (*catalog.Import, error) {
	st, p, err := s.reviewing(ctx, id)
	if err != nil {
		return nil, err
	}
	books, err := s.bookIndex(ctx)
	if err != nil {
		return nil, err
	}
	matches, err := s.matchesFor(ctx, st.Import.UserID, p, books)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var rollUp time.Time
	if s.retention != nil {
		rollUp = now.Add(-s.retention())
	}
	return s.cat.ApplyImport(ctx, id, func(st catalog.ImportState) (*catalog.ImportWrite, error) {
		w := plan(p, matches, st, s.loc, now)
		w.RollUpBefore, w.Zone = rollUp, s.loc
		return w, nil
	})
}
