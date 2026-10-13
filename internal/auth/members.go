package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Household members (player redesign Phase 8b-1, capability household_members):
// any non-demo person (the owner) adds a member from the player, with no admin
// and no invite. A member is a real account whose managed_by is the owner, with
// a member_kind (MemberKid or MemberGrownup) and a member_access (MemberAll or
// MemberCollections) that only the owner sets and changes. The three columns
// are all set or all NULL (SQLite can't add that CHECK with ALTER TABLE, so
// every write here sets or clears all three together). What a member may listen
// to follows from them in catalog (the member rule in shares.go); this file holds
// the accounts.
//
// Rules kept here:
//   - one level: an owner is never a member, and a member never has members;
//   - a member never gets the admin role (SetRole, AssignMember);
//   - a kid holds no password, API key, pairing token or recovery code
//     (clearKidCredentials), and its existing sign-ins stay;
//   - the owner's routes answer ErrNotMember for anyone who isn't their member,
//     so another person's member is indistinguishable from no such account.

// Member kinds and access.
const (
	MemberKid         = "kid"
	MemberGrownup     = "grownup"
	MemberAll         = "all"
	MemberCollections = "collections"
)

// MaxMemberName bounds a member's name, in characters after trimming.
const MaxMemberName = 40

// memberPairingTTL bounds the pairing token an owner mints for a member's own
// device: single use, like /auth/pair's.
const memberPairingTTL = 10 * time.Minute

// Errors of the member routes and the admin's member fields.
var (
	// ErrNotMember is a member id that isn't one of the caller's members (or no
	// such account): the transport answers 404.
	ErrNotMember = errors.New("member not found")
	// ErrMemberCannotManage is a member adding or managing members.
	ErrMemberCannotManage = errors.New("a household member can't manage household members")
	// ErrDemoAccount is a demo account adding members.
	ErrDemoAccount = errors.New("not available for demo accounts")
	// ErrFeatureOff is adding a member while the server's switch is off.
	ErrFeatureOff = errors.New("household members are turned off on this server")
	// ErrNotAllowed is adding a member while the admin turned the person's
	// switch off (can_add_members).
	ErrNotAllowed = errors.New("adding household members is turned off for your account")
	// ErrMemberLimit is adding a member at the cap.
	ErrMemberLimit = errors.New("you have the most household members you can add")
	// ErrInvalidName is a member name that is empty or too long after trimming,
	// or holds control characters or invalid UTF-8.
	ErrInvalidName = fmt.Errorf("the name must be 1 to %d characters, with no control characters", MaxMemberName)
	// ErrInvalidKind is a kind other than MemberKid or MemberGrownup.
	ErrInvalidKind = errors.New(`kind must be "kid" or "grownup"`)
	// ErrInvalidAccess is an access other than MemberAll or MemberCollections.
	ErrInvalidAccess = errors.New(`access must be "all" or "collections"`)
	// ErrMemberNotAdmin is making a member an admin, or an admin a member.
	ErrMemberNotAdmin = errors.New("a household member can't be an admin")
	// ErrInvalidKeepMembers is a DeleteUser keep id that isn't one of the
	// person's members.
	ErrInvalidKeepMembers = errors.New("keep_members names someone who isn't this person's member")
	// The admin's "Assign to someone" refusals.
	ErrOwnerIsMember = errors.New("the new owner is a household member")
	ErrOwnerIsDemo   = errors.New("the new owner is a demo account")
	ErrSelfManaged   = errors.New("a person can't manage themselves")
	ErrHasMembers    = errors.New("this person has household members of their own")
)

// MemberPolicy is the server's household settings as the transport reads them
// per request (config.Household).
type MemberPolicy struct {
	Enabled bool // household.members_enabled
	Max     int  // household.max_members
}

// Member is one of an owner's household members as the owner's list shows it.
// Name is the name the owner typed, else the username. CreatedAt is fixed-width
// millisecond UTC. Devices counts the member's live sessions and API keys.
type Member struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	Kind      string `json:"kind"`
	Access    string `json:"access"`
	CreatedAt string `json:"created_at"`
	Devices   int    `json:"devices"`
	// Disabled is for the admin console's members list (the player's has none).
	Disabled bool `json:"-"`
}

// ShareFunc shares the owner's chosen collections with a member inside the
// member write's own transaction (catalog.SetMemberCollections): auth never
// imports catalog, which owns the collections and their rules.
type ShareFunc func(tx *sql.Tx, memberID int64) error

// NewMember is what CreateMember makes.
type NewMember struct {
	Name, Kind, Access string
	// SignInHere also signs the member in on the owner's device: a session named
	// DeviceName recording OnToken (the owner's token) as created_on_token_id.
	SignInHere bool
	DeviceName string
	OnToken    int64
	// Share shares the owner's collections with the new member (nil: none).
	Share ShareFunc
}

// cleanMemberName trims a member's name and checks it: 1 to MaxMemberName
// characters, valid UTF-8, no control characters. ErrInvalidName otherwise.
func cleanMemberName(name string) (string, error) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n == 0 || n > MaxMemberName || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", ErrInvalidName
	}
	return name, nil
}

func checkKind(kind string) error {
	if kind != MemberKid && kind != MemberGrownup {
		return ErrInvalidKind
	}
	return nil
}

func checkAccess(access string) error {
	if access != MemberAll && access != MemberCollections {
		return ErrInvalidAccess
	}
	return nil
}

// maxUsernameBase bounds the part of a derived username taken from the name.
const maxUsernameBase = 24

// usernameSlug folds s to a username part: lower case ASCII letters and digits,
// any run of anything else one "-", no "-" at either end ("Évie Marie" ->
// "evie-marie"); "" when nothing is left.
func usernameSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range norm.NFKD.String(strings.ToLower(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		case unicode.Is(unicode.Mn, r):
			// A combining mark NFKD split off a letter ("é" -> "e" + U+0301): drop it.
		default:
			dash = true
		}
	}
	return b.String()
}

// usernameBase is the name's slug cut to maxUsernameBase bytes, or "member".
func usernameBase(name string) string {
	base := usernameSlug(name)
	if len(base) > maxUsernameBase {
		base = strings.TrimRight(base[:maxUsernameBase], "-")
	}
	if base == "" {
		return "member"
	}
	return base
}

// deriveUsername picks the member's username: the name's base, then the base
// with the owner's username, then numbered (-2 .. -99); the first one no account
// has in any case. Past -99 it is ErrUsernameTaken. Runs inside the create's
// transaction.
func deriveUsername(ctx context.Context, tx *sql.Tx, name, ownerUsername string) (string, error) {
	base := usernameBase(name)
	candidates := []string{base}
	if owner := usernameSlug(ownerUsername); owner != "" {
		candidates = append(candidates, base+"-"+owner)
	}
	for n := 2; n <= 99; n++ {
		candidates = append(candidates, fmt.Sprintf("%s-%d", base, n))
	}
	for _, c := range candidates {
		var taken bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE lower(username) = lower(?))`, c).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return c, nil
		}
	}
	return "", ErrUsernameTaken
}

// stampMillis renders a users.created_at (RFC3339, whole seconds) as the
// fixed-width millisecond UTC the /me lists use; unparsable stamps pass through.
func stampMillis(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// memberSelect reads Member rows (memberRow): ?1 is now (RFC3339) for the
// devices count, which takes the live sessions and API keys.
const memberSelect = `SELECT u.id, u.display_name, u.username, u.member_kind, u.member_access, u.created_at, u.disabled,
	       (SELECT COUNT(*) FROM tokens t WHERE t.user_id = u.id AND t.kind IN ('` + KindSession + `', '` + KindAPI + `')
	          AND t.revoked = 0 AND (t.expires_at IS NULL OR t.expires_at > ?1))
	  FROM users u`

// memberWhere keeps the rows that are members (all three columns set).
const memberWhere = ` u.managed_by IS NOT NULL AND u.member_kind IS NOT NULL AND u.member_access IS NOT NULL`

type rowScanner interface{ Scan(...any) error }

func scanMember(row rowScanner) (Member, error) {
	var m Member
	err := row.Scan(&m.ID, &m.Name, &m.Username, &m.Kind, &m.Access, &m.CreatedAt, &m.Disabled, &m.Devices)
	if m.Name == "" {
		m.Name = m.Username
	}
	m.CreatedAt = stampMillis(m.CreatedAt)
	return m, err
}

// rowQuerier is satisfied by *store.DB (its reader) and *sql.Tx.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// memberOf reads owner's member id, or ErrNotMember when id is anyone else (or
// no one).
func (s *Service) memberOf(ctx context.Context, q rowQuerier, owner, id int64) (Member, error) {
	m, err := scanMember(q.QueryRowContext(ctx,
		memberSelect+` WHERE u.id = ?2 AND u.managed_by = ?3 AND`+memberWhere, s.ts(), id, owner))
	if errors.Is(err, sql.ErrNoRows) || owner <= 0 {
		return Member{}, ErrNotMember
	}
	return m, err
}

// Member is owner's member id (ErrNotMember for anyone else).
func (s *Service) Member(ctx context.Context, owner, id int64) (Member, error) {
	return s.memberOf(ctx, s.db, owner, id)
}

// ListMembers returns owner's members, oldest first (created_at, then id).
func (s *Service) ListMembers(ctx context.Context, owner int64) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, memberSelect+` WHERE u.managed_by = ?2 AND`+memberWhere+`
	  ORDER BY u.created_at, u.id`, s.ts(), owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// checkOwner settles whether owner may add a member under policy, inside the
// create's transaction, in the order the transport answers them: a demo
// account, a member, the server's switch, the admin's switch (a disabled account
// too), the cap. It returns the owner's username.
func checkOwner(ctx context.Context, tx *sql.Tx, owner int64, policy MemberPolicy) (string, error) {
	var (
		username                    string
		demo, member, canAdd, inact bool
		count                       int
	)
	err := tx.QueryRowContext(ctx,
		`SELECT username, is_demo, managed_by IS NOT NULL, can_add_members, disabled,
		        (SELECT COUNT(*) FROM users m WHERE m.managed_by = ?1)
		   FROM users WHERE id = ?1`, owner).Scan(&username, &demo, &member, &canAdd, &inact, &count)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", err
	case demo:
		return "", ErrDemoAccount
	case member:
		return "", ErrMemberCannotManage
	case !policy.Enabled:
		return "", ErrFeatureOff
	case !canAdd || inact:
		return "", ErrNotAllowed
	case count >= policy.Max:
		return "", ErrMemberLimit
	}
	return username, nil
}

// CreateMember adds a household member for owner, in one transaction: it checks
// the owner may (checkOwner) and the input (name, kind, access), derives a
// unique username, inserts the account (role user, no password, the three member
// columns, the typed name), shares the owner's collections (in.Share) and, with
// in.SignInHere, signs the member in on the owner's device. It returns the member
// and the session's secret ("" without SignInHere).
func (s *Service) CreateMember(ctx context.Context, owner int64, in NewMember, policy MemberPolicy) (Member, string, error) {
	var (
		m      Member
		secret string
	)
	err := s.db.WithTx(ctx, "CreateMember", func(tx *sql.Tx) error {
		ownerName, err := checkOwner(ctx, tx, owner, policy)
		if err != nil {
			return err
		}
		name, err := cleanMemberName(in.Name)
		if err != nil {
			return err
		}
		if err := checkKind(in.Kind); err != nil {
			return err
		}
		if err := checkAccess(in.Access); err != nil {
			return err
		}
		username, err := deriveUsername(ctx, tx, name, ownerName)
		if err != nil {
			return err
		}
		now := s.ts()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO users(username, password_hash, role, is_demo, created_at, updated_at,
			                   managed_by, member_kind, member_access, display_name)
			 VALUES(?, '', ?, 0, ?, ?, ?, ?, ?, ?)`,
			username, RoleUser, now, now, owner, in.Kind, in.Access, name)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if in.Share != nil {
			if err := in.Share(tx, id); err != nil {
				return err
			}
		}
		if in.SignInHere {
			if secret, err = s.memberSessionTx(ctx, tx, id, in.DeviceName, in.OnToken); err != nil {
				return err
			}
		}
		m, err = s.memberOf(ctx, tx, owner, id)
		return err
	})
	if err != nil {
		return Member{}, "", err
	}
	return m, secret, nil
}

// memberSessionTx issues a session for member, named deviceName and recording
// onToken (0: none) as the device it was made on.
func (s *Service) memberSessionTx(ctx context.Context, ex sqlExecer, member int64, deviceName string, onToken int64) (string, error) {
	secret, hash, err := generateToken()
	if err != nil {
		return "", err
	}
	var on any
	if onToken > 0 {
		on = onToken
	}
	if _, err := s.insertTokenOn(ctx, ex, member, hash, KindSession, deviceName, nil, nil, "", on); err != nil {
		return "", err
	}
	return secret, nil
}

// MemberSession signs owner's member id in on the owner's device: a session
// named deviceName recording onToken (the owner's token). It returns the secret
// and the member's account (for the session envelope).
func (s *Service) MemberSession(ctx context.Context, owner, id int64, deviceName string, onToken int64) (string, *User, error) {
	var secret string
	err := s.db.WithTx(ctx, "MemberSession", func(tx *sql.Tx) error {
		if _, err := s.memberOf(ctx, tx, owner, id); err != nil {
			return err
		}
		var err error
		secret, err = s.memberSessionTx(ctx, tx, id, deviceName, onToken)
		return err
	})
	if err != nil {
		return "", nil, err
	}
	u, err := s.GetUser(ctx, id)
	return secret, u, err
}

// MemberPairing mints a pairing token for owner's member id, for the member's
// own device: unlinked, single use, memberPairingTTL, like /auth/pair's.
func (s *Service) MemberPairing(ctx context.Context, owner, id int64) (string, error) {
	if _, err := s.Member(ctx, owner, id); err != nil {
		return "", err
	}
	return s.IssueToken(ctx, id, KindPairing, "", memberPairingTTL)
}

// MemberDevices lists owner's member id's live sessions and API keys.
func (s *Service) MemberDevices(ctx context.Context, owner, id int64) ([]Device, error) {
	if _, err := s.Member(ctx, owner, id); err != nil {
		return nil, err
	}
	return s.ListDevices(ctx, id)
}

// RevokeMemberDevice signs one of owner's member id's devices out: ErrNotMember
// for anyone else's member, ErrNotFound for a device that isn't the member's
// live session or API key.
func (s *Service) RevokeMemberDevice(ctx context.Context, owner, id, deviceID int64) error {
	if _, err := s.Member(ctx, owner, id); err != nil {
		return err
	}
	return s.RevokeOwnDevice(ctx, id, deviceID)
}

// MemberUpdate is an owner's change to a member: nil leaves a field.
type MemberUpdate struct {
	Name, Kind, Access *string
	// Share replaces the owner's collections shared with the member (nil: no
	// change).
	Share ShareFunc
}

// UpdateMember changes owner's member id in one transaction and returns the
// member as it is now plus what changed (name, kind, access: the new values of
// those that differ). Switching a grown-up to a kid clears their credentials
// (clearKidCredentials); signed-in devices stay signed in. Anyone else's member
// is ErrNotMember, settled before the input is checked.
func (s *Service) UpdateMember(ctx context.Context, owner, id int64, in MemberUpdate) (Member, map[string]any, error) {
	var (
		m       Member
		changes = map[string]any{}
	)
	err := s.db.WithTx(ctx, "UpdateMember", func(tx *sql.Tx) error {
		cur, err := s.memberOf(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		name := cur.Name
		if in.Name != nil {
			if name, err = cleanMemberName(*in.Name); err != nil {
				return err
			}
		}
		kind, access := cur.Kind, cur.Access
		if in.Kind != nil {
			if kind = *in.Kind; checkKind(kind) != nil {
				return ErrInvalidKind
			}
		}
		if in.Access != nil {
			if access = *in.Access; checkAccess(access) != nil {
				return ErrInvalidAccess
			}
		}
		if name != cur.Name {
			changes["name"] = name
		}
		if kind != cur.Kind {
			changes["kind"] = kind
		}
		if access != cur.Access {
			changes["access"] = access
		}
		if len(changes) > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET display_name = ?, member_kind = ?, member_access = ?, updated_at = ? WHERE id = ?`,
				name, kind, access, s.ts(), id); err != nil {
				return err
			}
		}
		if kind == MemberKid && cur.Kind != MemberKid {
			if err := clearKidCredentials(ctx, tx, id); err != nil {
				return err
			}
		}
		if in.Share != nil {
			if err := in.Share(tx, id); err != nil {
				return err
			}
		}
		m, err = s.memberOf(ctx, tx, owner, id)
		return err
	})
	if err != nil {
		return Member{}, nil, err
	}
	return m, changes, nil
}

// clearKidCredentials takes from a member becoming a kid every way to sign in
// that a kid can't have (plan decision 1): the password (and the browsers it
// signed in from), API keys, open pairing tokens and the recovery code (whose
// linked pairing tokens cascade). Sessions stay: their devices stay signed in.
func clearKidCredentials(ctx context.Context, tx *sql.Tx, id int64) error {
	for _, q := range []string{
		`UPDATE users SET password_hash = '' WHERE id = ?1`,
		`UPDATE tokens SET revoked = 1 WHERE user_id = ?1 AND kind IN ('` + KindAPI + `', '` + KindPairing + `') AND revoked = 0`,
		`DELETE FROM auth_codes WHERE user_id = ?1 AND kind = '` + CodeRecovery + `'`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return forgetBrowsers(ctx, tx, id)
}

// DeleteMember deletes owner's member id and all of its state (the schema's
// cascade, as DeleteUser), returning the member as it was.
func (s *Service) DeleteMember(ctx context.Context, owner, id int64) (Member, error) {
	var m Member
	err := s.db.WithTx(ctx, "DeleteMember", func(tx *sql.Tx) error {
		var err error
		if m, err = s.memberOf(ctx, tx, owner, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
		return err
	})
	return m, err
}

// memberNames maps a person's members' ids to their usernames.
func memberNames(ctx context.Context, tx *sql.Tx, owner int64) (map[int64]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, username FROM users WHERE managed_by = ?`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// MemberSignIn names a member signed in on one of their owner's devices.
type MemberSignIn struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// MemberSignIns maps each of owner's devices (token ids) to the owner's members
// holding a live session or API key made on it (created_on_token_id), by name.
// Only owner's own members appear.
func (s *Service) MemberSignIns(ctx context.Context, owner int64) (map[int64][]MemberSignIn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT t.created_on_token_id, u.id, CASE WHEN u.display_name <> '' THEN u.display_name ELSE u.username END
		   FROM tokens t JOIN users u ON u.id = t.user_id
		  WHERE u.managed_by = ? AND t.created_on_token_id IS NOT NULL AND t.kind IN (?, ?) AND t.revoked = 0
		    AND (t.expires_at IS NULL OR t.expires_at > ?)
		  ORDER BY t.created_on_token_id, u.created_at, u.id`, owner, KindSession, KindAPI, s.ts())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]MemberSignIn{}
	for rows.Next() {
		var (
			device int64
			m      MemberSignIn
		)
		if err := rows.Scan(&device, &m.ID, &m.Name); err != nil {
			return nil, err
		}
		out[device] = append(out[device], m)
	}
	return out, rows.Err()
}

// SetCanAddMembers sets the admin's per-person switch.
func (s *Service) SetCanAddMembers(ctx context.Context, id int64, on bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET can_add_members = ?, updated_at = ? WHERE id = ?`, on, s.ts(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AssignMember makes account id a household member of owner, with kind and
// access (the admin's "Assign to someone"). Refused: an unknown account or owner
// (ErrNotFound), the same person (ErrSelfManaged), an owner who is a member
// (ErrOwnerIsMember) or a demo account (ErrOwnerIsDemo), an admin as the member
// (ErrMemberNotAdmin), a person with members of their own (ErrHasMembers: no
// chains). The owner's cap and switch don't apply: the admin decides. As a kid,
// the person's credentials are cleared (clearKidCredentials). Their own grants
// stay (the member rule is a union).
func (s *Service) AssignMember(ctx context.Context, id, owner int64, kind, access string) error {
	if err := checkKind(kind); err != nil {
		return err
	}
	if err := checkAccess(access); err != nil {
		return err
	}
	if id == owner {
		return ErrSelfManaged
	}
	return s.db.WithTx(ctx, "AssignMember", func(tx *sql.Tx) error {
		var (
			role    string
			members int
		)
		err := tx.QueryRowContext(ctx,
			`SELECT role, (SELECT COUNT(*) FROM users m WHERE m.managed_by = u.id) FROM users u WHERE u.id = ?`, id).
			Scan(&role, &members)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var ownerMember, ownerDemo bool
		err = tx.QueryRowContext(ctx, `SELECT managed_by IS NOT NULL, is_demo FROM users WHERE id = ?`, owner).
			Scan(&ownerMember, &ownerDemo)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case ownerMember:
			return ErrOwnerIsMember
		case ownerDemo:
			return ErrOwnerIsDemo
		case role == RoleAdmin:
			return ErrMemberNotAdmin
		case members > 0:
			return ErrHasMembers
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET managed_by = ?, member_kind = ?, member_access = ?, updated_at = ? WHERE id = ?`,
			owner, kind, access, s.ts(), id); err != nil {
			return err
		}
		if kind == MemberKid {
			return clearKidCredentials(ctx, tx, id)
		}
		return nil
	})
}

// MakeIndependent turns member id into an ordinary account (the admin's "Make
// independent"): the three member columns cleared, so they keep only their own
// grants. A no-op for an account that isn't a member; ErrNotFound for no account.
func (s *Service) MakeIndependent(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET managed_by = NULL, member_kind = NULL, member_access = NULL,
		        updated_at = CASE WHEN managed_by IS NULL THEN updated_at ELSE ? END
		  WHERE id = ?`, s.ts(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
