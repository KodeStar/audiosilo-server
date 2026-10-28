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
//   - a member never gets the admin role (UpdateUser);
//   - a kid holds no password, API key, pairing token, recovery code, invite or
//     house sharing, and shares collections only with their owner: becoming one
//     clears them (clearKidCredentials, its existing sign-ins stay), and the
//     writes that would make one refuse a kid (refuseKid, UpdateUser);
//   - leaving an owner's household, or joining another's, signs the person out
//     of what another owner set up for them (leaveOwners);
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
	ErrMemberIsDemo  = errors.New("a demo account can't be a household member")
	// ErrKidNoPassword is a password set on a kid, or in the same change that
	// assigns the person as a kid: a kid holds none.
	ErrKidNoPassword = errors.New("a kid's account has no password")
	// ErrMemberDisabled is signing in, or pairing a device for, a member whose
	// account the admin disabled: no token of theirs would work.
	ErrMemberDisabled = errors.New("this household member's account is disabled")
	// ErrKidAccount is a credential a kid can't hold, refused inside the write
	// that would make it (refuseKid): the transport's notKid answers first, but a
	// request that passed it just before the owner's switch to kid committed
	// lands here.
	ErrKidAccount = errors.New("not available for a kid's account")
)

// refuseKid is ErrKidAccount when id is a kid (all three member columns, as
// User.IsKid reads them), read through q: the write's own transaction on the
// one writer, so no switch to kid can commit between the check and the write.
func refuseKid(ctx context.Context, q rowQuerier, id int64) error {
	var kid bool
	err := q.QueryRowContext(ctx, `SELECT COALESCE(member_kind = '`+MemberKid+`' AND managed_by IS NOT NULL
	        AND member_access IS NOT NULL, 0) FROM users WHERE id = ?`, id).Scan(&kid)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	case kid:
		return ErrKidAccount
	}
	return nil
}

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
// with the owner's username, then numbered (-2, -3, ...); the first one no
// account has in any case (one read of which are taken, so there is always a
// free one: a server whose people have names with no Latin letters shares the
// base "member"). Runs inside the create's transaction.
func deriveUsername(ctx context.Context, tx *sql.Tx, name, ownerUsername string) (string, error) {
	base := usernameBase(name)
	// The base is lower case ASCII letters, digits and '-' (usernameSlug), so
	// lower() on the stored side is the whole case fold, and the LIKE pattern
	// holds no wildcard but its own '%'.
	rows, err := tx.QueryContext(ctx,
		`SELECT lower(username) FROM users WHERE lower(username) = ?1 OR lower(username) LIKE ?1 || '-%'`, base)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	taken := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return "", err
		}
		taken[u] = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if !taken[base] {
		return base, nil
	}
	if owner := usernameSlug(ownerUsername); owner != "" && !taken[base+"-"+owner] {
		return base + "-" + owner, nil
	}
	for n := 2; ; n++ {
		if c := fmt.Sprintf("%s-%d", base, n); !taken[c] {
			return c, nil
		}
	}
}

// stampMillis renders a users.created_at (RFC3339, whole seconds) as the
// fixed-width millisecond UTC the /me lists use; unparsable stamps pass through.
func stampMillis(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(millisUTC)
}

// memberSelect reads Member rows (scanMember): ?1 is now (RFC3339) for the
// devices count, which takes the live sessions and API keys.
const memberSelect = `SELECT u.id, ` + memberNameSQL + `, u.username, u.member_kind, u.member_access, u.created_at, u.disabled,
	       (SELECT COUNT(*) FROM tokens t WHERE t.user_id = u.id AND ` + liveDeviceSQL + `)
	  FROM users u`

// memberNameSQL is a member u's name as their owner sees it: the name the owner
// typed, else the username.
const memberNameSQL = `CASE WHEN u.display_name <> '' THEN u.display_name ELSE u.username END`

// liveDeviceSQL keeps the tokens t that are a device: a live (not revoked, not
// expired at ?1, now in RFC3339) session or API key.
const liveDeviceSQL = `t.kind IN ('` + KindSession + `', '` + KindAPI + `') AND t.revoked = 0
	AND (t.expires_at IS NULL OR t.expires_at > ?1)`

// memberWhere keeps the rows that are members (all three columns set).
const memberWhere = ` u.managed_by IS NOT NULL AND u.member_kind IS NOT NULL AND u.member_access IS NOT NULL`

type rowScanner interface{ Scan(...any) error }

func scanMember(row rowScanner) (Member, error) {
	var m Member
	err := row.Scan(&m.ID, &m.Name, &m.Username, &m.Kind, &m.Access, &m.CreatedAt, &m.Disabled, &m.Devices)
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
	if errors.Is(err, sql.ErrNoRows) {
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

// MayAddMembers settles whether u may add a household member under policy,
// leaving out the cap (which a list shows as a count, not a refusal), in the
// order the transport answers them: a demo account (ErrDemoAccount), a member
// (ErrMemberCannotManage), the server's switch (ErrFeatureOff), the admin's
// switch or a disabled account (ErrNotAllowed). It reads nothing: u is the
// account as GetUser reads it.
func MayAddMembers(u *User, policy MemberPolicy) error {
	switch {
	case u.IsDemo:
		return ErrDemoAccount
	case u.IsMember():
		return ErrMemberCannotManage
	case !policy.Enabled:
		return ErrFeatureOff
	case !u.CanAddMembers || u.Disabled:
		return ErrNotAllowed
	}
	return nil
}

// checkOwner is MayAddMembers then the cap (ErrMemberLimit) for owner, read
// inside the create's transaction. It returns the owner's account.
func checkOwner(ctx context.Context, tx *sql.Tx, owner int64, policy MemberPolicy) (*User, error) {
	u, err := getUser(ctx, tx, owner)
	if err != nil {
		return nil, err
	}
	if err := MayAddMembers(u, policy); err != nil {
		return nil, err
	}
	if u.MembersCount >= policy.Max {
		return nil, ErrMemberLimit
	}
	return u, nil
}

// CreateMember adds a household member for owner, in one transaction: it checks
// the owner may (checkOwner) and the input (name, kind, access), derives a
// unique username, inserts the account (role user, no password, the three member
// columns, the typed name), shares the owner's collections (in.Share) and, with
// in.SignInHere, signs the member in on the owner's device. It returns the member
// and, with SignInHere, the session's secret and the member's account (for the
// session envelope); "" and nil without.
func (s *Service) CreateMember(ctx context.Context, owner int64, in NewMember, policy MemberPolicy) (Member, string, *User, error) {
	var (
		m      Member
		secret string
		u      *User
	)
	err := s.db.WithTx(ctx, "CreateMember", func(tx *sql.Tx) error {
		o, err := checkOwner(ctx, tx, owner, policy)
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
		username, err := deriveUsername(ctx, tx, name, o.Username)
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
			if u, err = getUser(ctx, tx, id); err != nil {
				return err
			}
		}
		m, err = s.memberOf(ctx, tx, owner, id)
		return err
	})
	if err != nil {
		return Member{}, "", nil, err
	}
	return m, secret, u, nil
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
// and the member's account (for the session envelope). A member the admin
// disabled is ErrMemberDisabled: no token of theirs would work.
func (s *Service) MemberSession(ctx context.Context, owner, id int64, deviceName string, onToken int64) (string, *User, error) {
	var (
		secret string
		u      *User
	)
	err := s.db.WithTx(ctx, "MemberSession", func(tx *sql.Tx) error {
		m, err := s.memberOf(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		if m.Disabled {
			return ErrMemberDisabled
		}
		if secret, err = s.memberSessionTx(ctx, tx, id, deviceName, onToken); err != nil {
			return err
		}
		u, err = getUser(ctx, tx, id)
		return err
	})
	if err != nil {
		return "", nil, err
	}
	return secret, u, nil
}

// MemberPairing mints a pairing token for owner's member id, for the member's
// own device: unlinked, single use, valid for ttl (the transport's /auth/pair
// lifetime). It returns the member with the token. A member the admin disabled
// is ErrMemberDisabled, as MemberSession.
func (s *Service) MemberPairing(ctx context.Context, owner, id int64, ttl time.Duration) (Member, string, error) {
	var (
		m     Member
		token string
	)
	err := s.db.WithTx(ctx, "MemberPairing", func(tx *sql.Tx) error {
		var err error
		if m, err = s.memberOf(ctx, tx, owner, id); err != nil {
			return err
		}
		if m.Disabled {
			return ErrMemberDisabled
		}
		token, err = s.issueToken(ctx, tx, id, KindPairing, "", ttl)
		return err
	})
	if err != nil {
		return Member{}, "", err
	}
	return m, token, nil
}

// MemberDevices lists owner's member id's live sessions and API keys.
func (s *Service) MemberDevices(ctx context.Context, owner, id int64) ([]Device, error) {
	if _, err := s.Member(ctx, owner, id); err != nil {
		return nil, err
	}
	return s.ListDevices(ctx, id)
}

// RevokeMemberDevice signs one of owner's member id's devices out, in one
// transaction, and returns the member: ErrNotMember for anyone else's member,
// ErrNotFound for a device that isn't the member's live session or API key.
func (s *Service) RevokeMemberDevice(ctx context.Context, owner, id, deviceID int64) (Member, error) {
	var m Member
	err := s.db.WithDurableTx(ctx, "RevokeMemberDevice", func(tx *sql.Tx) error {
		var err error
		if m, err = s.memberOf(ctx, tx, owner, id); err != nil {
			return err
		}
		_, err = revokeDeviceTx(ctx, tx, deviceID, id, false)
		return err
	})
	if err != nil {
		return Member{}, err
	}
	return m, nil
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
	err := s.db.WithDurableTx(ctx, "UpdateMember", func(tx *sql.Tx) error {
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
			kind = *in.Kind
			if err := checkKind(kind); err != nil {
				return err
			}
		}
		if in.Access != nil {
			access = *in.Access
			if err := checkAccess(access); err != nil {
				return err
			}
		}
		// The typed name only when it changes: cur.Name falls back to the
		// username (memberNameSQL), which must not become a typed name of its own.
		var nameArg any
		if name != cur.Name {
			changes["name"] = name
			nameArg = name
		}
		if kind != cur.Kind {
			changes["kind"] = kind
		}
		if access != cur.Access {
			changes["access"] = access
		}
		if len(changes) > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET display_name = COALESCE(?, display_name), member_kind = ?, member_access = ?, updated_at = ?
				  WHERE id = ?`,
				nameArg, kind, access, s.ts(), id); err != nil {
				return err
			}
		}
		if kind == MemberKid && cur.Kind != MemberKid {
			if err := clearKidCredentials(ctx, tx, id, s.ts()); err != nil {
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
// signed in from), API keys, open pairing tokens, the recovery code and any
// still-redeemable invite (whose linked pairing tokens cascade), so no code can
// pair a new device into the kid's account. Sessions stay: their devices stay
// signed in. It also clears their household listening choice
// (household_sharing): a kid never shares nor sees the house (catalog's
// household.go relies on no kid having a row, which the routes keep: a kid can't
// set one), so a kid made a grown-up again starts from off and opts in again.
// And it takes away the collections shared between the kid and anyone but their
// owner (managed_by, already the new owner when the admin assigns), both ways: a
// kid is shared with only by their owner and shares with nobody else (catalog's
// kidOnlyToOwnerSQL, ShareTargets), so a sharer is never left with a kid in the
// list they are shown that the server would refuse back.
func clearKidCredentials(ctx context.Context, tx *sql.Tx, id int64, now string) error {
	for _, q := range []string{
		`UPDATE users SET password_hash = '' WHERE id = ?1`,
		`UPDATE tokens SET revoked = 1 WHERE user_id = ?1 AND kind IN ('` + KindAPI + `', '` + KindPairing + `') AND revoked = 0`,
		`DELETE FROM auth_codes WHERE user_id = ?1 AND kind = '` + CodeRecovery + `'`,
		`DELETE FROM household_sharing WHERE user_id = ?1`,
		`DELETE FROM collection_shares
		  WHERE (user_id = ?1 AND collection_id IN (SELECT id FROM collections
		          WHERE user_id IS NOT (SELECT managed_by FROM users WHERE id = ?1)))
		     OR (collection_id IN (SELECT id FROM collections WHERE user_id = ?1)
		          AND user_id IS NOT (SELECT managed_by FROM users WHERE id = ?1))`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	if err := supersedeActiveInvites(ctx, tx, id, now); err != nil {
		return err
	}
	return forgetBrowsers(ctx, tx, id)
}

// leaveOwners signs a person who leaves their owner's household (newOwner 0:
// "Make independent", or kept when their owner is deleted) or joins newOwner's
// out of what another owner set up for them: the open pairing codes minted for
// their own device (unlinked pairing tokens) and, joining newOwner's household,
// the sessions and API keys made on anyone else's devices (created_on_token_id),
// which would otherwise carry newOwner's access to that person's phone. Their own
// devices stay signed in, and so, made independent, do the devices their old
// owner signed them in on (the account then reaches only its own grants).
func leaveOwners(ctx context.Context, tx *sql.Tx, id, newOwner int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE tokens SET revoked = 1
	  WHERE user_id = ?1 AND revoked = 0
	    AND ((kind = '`+KindPairing+`' AND auth_code_id IS NULL)
	         OR (?2 > 0 AND kind IN ('`+KindSession+`', '`+KindAPI+`')
	             AND (SELECT d.user_id FROM tokens d WHERE d.id = tokens.created_on_token_id) <> ?2))`, id, newOwner)
	return err
}

// DeleteMember deletes owner's member id and all of its state (the schema's
// cascade, as DeleteUser), returning the member as it was.
func (s *Service) DeleteMember(ctx context.Context, owner, id int64) (Member, error) {
	var m Member
	err := s.db.WithDurableTx(ctx, "DeleteMember", func(tx *sql.Tx) error {
		var err error
		if m, err = s.memberOf(ctx, tx, owner, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
		return err
	})
	return m, err
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
		`SELECT DISTINCT t.created_on_token_id, u.id, `+memberNameSQL+`
		   FROM tokens t JOIN users u ON u.id = t.user_id
		  WHERE u.managed_by = ?2 AND t.created_on_token_id IS NOT NULL AND `+liveDeviceSQL+`
		  ORDER BY t.created_on_token_id, u.created_at, u.id`, s.ts(), owner)
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

// checkAssign settles HouseholdChange's refusals of u joining owner's
// household, inside UpdateUser's transaction (the input and ErrSelfManaged are
// checked before it).
func checkAssign(ctx context.Context, tx *sql.Tx, u *User, owner int64) error {
	var ownerMember, ownerDemo bool
	err := tx.QueryRowContext(ctx, `SELECT managed_by IS NOT NULL, is_demo FROM users WHERE id = ?`, owner).
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
	case u.IsDemo:
		return ErrMemberIsDemo
	case u.Role == RoleAdmin:
		return ErrMemberNotAdmin
	case u.MembersCount > 0:
		return ErrHasMembers
	}
	return nil
}
