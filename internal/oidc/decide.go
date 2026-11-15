package oidc

// Account is what Decide needs to know of an account.
type Account struct {
	ID       int64
	Admin    bool
	Disabled bool
	Demo     bool
	Member   bool // a household member (managed_by set): never linked
	// Linked: the account already has an identity from this issuer (another
	// subject), so a second can't be linked.
	Linked bool
}

// eligible reports whether a verified email may link a sign-in to a: not a demo,
// a member, disabled or already linked to this issuer. An admin is eligible here
// and set aside by Decide (decision 4).
func (a Account) eligible() bool { return !a.Demo && !a.Member && !a.Disabled && !a.Linked }

// Policy is the console's live sign-in settings.
type Policy struct {
	AutoCreate   bool
	AllowedGroup string
	AdminGroup   string
}

// Input is everything Decide reads: the sign-in's claims, the account its
// (issuer, subject) is linked to (nil when none), and, when none, the accounts
// whose email equals the sign-in's (MatchEmail), whatever their state.
type Input struct {
	Claims Claims
	Linked *Account
	// GroupsSeen: the linked identity has signed in with a groups claim
	// before, even an empty list (or was linked from a waiting row holding a
	// group), so the provider sends one for this person and a sign-in without
	// it means no groups. Always false with no Linked.
	GroupsSeen bool
	Matches    []Account
	Policy     Policy
}

// Outcome is what a sign-in does.
type Outcome string

// Outcomes.
const (
	SignIn  Outcome = "sign_in" // the linked account signs in
	Link    Outcome = "link"    // link to the one eligible account by email, then sign in
	Create  Outcome = "create"  // make an ordinary account, link it, sign in
	Pending Outcome = "pending" // record the sign-in for an admin; no session
	Refuse  Outcome = "refuse"  // no session, nothing recorded
)

// Reasons: Pending's (oidc_pending.reason) and Refuse's.
const (
	ReasonUnverifiedEmail = "unverified_email" // the email matches an account but isn't verified
	ReasonNoMatch         = "no_match"         // no verified email to match on, auto_create off
	ReasonSeveralMatches  = "several_matches"  // the verified email matches more than one eligible account
	ReasonNotEligible     = "not_eligible"     // it matches only accounts that can't be linked
	ReasonNotAllowed      = "not_allowed"      // auto_create on, but not in allowed_group
	ReasonAutoCreateOff   = "auto_create_off"  // a verified email matching no account, auto_create off
	ReasonAdminAccount    = "admin_account"    // it matches an admin, never linked by email (decision 4)
	ReasonDisabled        = "disabled"         // Refuse: the linked account is disabled
	ReasonMember          = "member"           // Refuse: the linked account has become a household member
)

// Decision is Decide's answer.
type Decision struct {
	Outcome Outcome
	// Account is the account signing in (SignIn, Link), else 0.
	Account int64
	// Reason is Pending's or Refuse's.
	Reason string
	// Role is the role the account should have after this sign-in (the
	// admin_group rule), "" for no change of role by the sign-in.
	Role string
	// NoGroups: admin_group is set but the sign-in carried no groups claim at
	// all (neither the ID token nor userinfo; an empty list is a claim) and
	// the identity has never carried one (Input.GroupsSeen), so the role is
	// left as it is: a provider that never sends the claim (no scope or mapper
	// for it) demotes no one. Once the identity has carried it, an absent
	// claim is no groups (some providers, Keycloak among them, leave the claim
	// out for a person in no group).
	NoGroups bool
}

// Roles Decide speaks of (auth's).
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Decide says what a sign-in does (decisions 1-4):
//   - linked: sign in, unless the account is disabled or now a member;
//   - a verified email matching an admin: pending (never linked by email);
//   - a verified email matching exactly one eligible account: link it;
//   - a verified email matching others (several, or none eligible): pending;
//   - an unverified email matching an account: pending;
//   - otherwise, with auto_create on: create an account, if allowed_group (when
//     set) lists the person, else pending not_allowed; with it off: pending.
//
// Never on a username. Role is set for every outcome that signs in while
// admin_group is set, unless the sign-in carried no groups claim and its
// identity never has (NoGroups); allowed_group, read only for a sign-in with
// no identity yet, counts an absent claim as no groups.
func Decide(in Input) Decision {
	d := decide(in)
	if (d.Outcome == SignIn || d.Outcome == Link || d.Outcome == Create) && in.Policy.AdminGroup != "" {
		if in.Claims.Groups == nil && !in.GroupsSeen {
			d.NoGroups = true
			return d
		}
		d.Role = RoleUser
		if in.Claims.InGroup(in.Policy.AdminGroup) {
			d.Role = RoleAdmin
		}
	}
	return d
}

func decide(in Input) Decision {
	if a := in.Linked; a != nil {
		switch {
		case a.Disabled:
			return Decision{Outcome: Refuse, Reason: ReasonDisabled}
		case a.Member || a.Demo:
			return Decision{Outcome: Refuse, Reason: ReasonMember}
		}
		return Decision{Outcome: SignIn, Account: a.ID}
	}
	c := in.Claims
	if c.Email != "" && len(in.Matches) > 0 {
		if !c.EmailVerified {
			return Decision{Outcome: Pending, Reason: ReasonUnverifiedEmail}
		}
		var eligible []Account
		for _, m := range in.Matches {
			if m.Admin {
				return Decision{Outcome: Pending, Reason: ReasonAdminAccount}
			}
			if m.eligible() {
				eligible = append(eligible, m)
			}
		}
		switch len(eligible) {
		case 1:
			return Decision{Outcome: Link, Account: eligible[0].ID}
		case 0:
			return Decision{Outcome: Pending, Reason: ReasonNotEligible}
		default:
			return Decision{Outcome: Pending, Reason: ReasonSeveralMatches}
		}
	}
	if in.Policy.AutoCreate {
		if g := in.Policy.AllowedGroup; g != "" && !c.InGroup(g) {
			return Decision{Outcome: Pending, Reason: ReasonNotAllowed}
		}
		return Decision{Outcome: Create}
	}
	if c.Email != "" && c.EmailVerified {
		return Decision{Outcome: Pending, Reason: ReasonAutoCreateOff}
	}
	return Decision{Outcome: Pending, Reason: ReasonNoMatch}
}
