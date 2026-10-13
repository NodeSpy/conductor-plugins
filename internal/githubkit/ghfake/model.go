package ghfake

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The in-memory GitHub. One mutex (Fake.mu) guards everything below; handlers
// and scenario methods take it for the whole operation, so every observable
// change is atomic, like a GitHub write.

// User is an account: a person, a Bot (an App's bot account, `cursor[bot]`),
// or an Organization.
type User struct {
	ID    int64
	Login string
	Type  string // User | Bot | Organization
	Name  string
	Email string
}

// Protection is a base branch's protection rules, as far as mergeability
// depends on them.
type Protection struct {
	// RequiredChecks are check-run names / status contexts that must succeed.
	RequiredChecks []string
	// RequiredApprovals is the approving-review count merging needs.
	RequiredApprovals int
	// RequireUpToDate: the head must contain the base's latest commit
	// (otherwise mergeable_state is "behind").
	RequireUpToDate bool
	// RequireConversationResolution: unresolved review threads block.
	RequireConversationResolution bool
}

// Repo is one repository.
type Repo struct {
	ID            int64
	Owner         *User
	Name          string
	DefaultBranch string
	Private       bool
	Protection    Protection
	// InstallationID is the App installation that covers this repo (0: none).
	InstallationID int64
	// Collaborators may be requested as reviewers (lowercase logins).
	Collaborators map[string]bool

	Branches   map[string]string // ref → head sha
	Commits    map[string]*Commit
	nextNumber int
	Issues     map[int]*Issue // PRs are issues too (shared numbering)
	Labels     map[string]*Label
	Statuses   []*Status
	CheckRuns  []*CheckRun
	Suites     []*CheckSuite
	Runs       []*WorkflowRun
	Jobs       []*Job
	Releases   []*Release
	Contents   map[string]*Content
	CreatedAt  time.Time
}

// FullName is owner/name.
func (r *Repo) FullName() string { return r.Owner.Login + "/" + r.Name }

// Commit is one commit.
type Commit struct {
	SHA     string
	Message string
	Parents []string
	Author  *User
	At      time.Time
}

// Label is a repo label.
type Label struct {
	ID    int64
	Name  string
	Color string
}

// Content is one file in the default branch (the contents API).
type Content struct {
	Path string
	Text string
	SHA  string
}

// Issue is an issue — or the issue half of a pull request.
type Issue struct {
	ID          int64
	Number      int
	Title       string
	Body        string
	State       string // open | closed
	StateReason string
	User        *User
	Labels      []string
	Assignees   []string
	Comments    []*IssueComment
	Pull        *Pull // non-nil: this issue is a PR
	Locked      bool
	// ProjectFields are the issue's Projects v2 single-select field values
	// (field name → option name); non-empty means the issue is on a project.
	ProjectFields map[string]string
	// LinkedBranches / ClosedBy: branches linked to the issue, and PR numbers
	// that will close it.
	LinkedBranches []string
	ClosedBy       []int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ClosedAt       time.Time
}

// File is one changed file of a PR, with the new-side lines its diff covers
// (an inline review comment must land on one of them).
type File struct {
	Path      string
	Status    string // added | modified | removed
	Additions int
	Deletions int
	Lines     []int
}

// Pull is the pull-request half of an Issue.
type Pull struct {
	ID        int64
	Issue     *Issue
	Draft     bool
	HeadRef   string
	BaseRef   string
	HeadSHA   string
	MergeBase string // the base sha the head branched from (or last caught up with)
	Conflict  bool   // the head conflicts with the base (mergeable_state dirty)
	Files     []File

	Merged         bool
	MergedAt       time.Time
	MergedBy       *User
	MergeCommitSHA string

	RequestedUsers []string // lowercase logins with a pending review request
	RequestedTeams []string
	Reviews        []*Review
	ReviewComments []*ReviewComment
	Threads        []*Thread

	// mergeComputedAt counts the reads since the head or base last moved:
	// GitHub computes mergeability asynchronously, so the first read after a
	// change answers null / "unknown" (Options.MergeabilityDelay).
	mergeReads int
}

// Review is a submitted (or pending) pull-request review.
type Review struct {
	ID          int64
	User        *User
	State       string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	Body        string
	CommitID    string
	SubmittedAt time.Time
	Comments    []*ReviewComment
	Reactions   []*Reaction
}

// ReviewComment is an inline (diff) comment.
type ReviewComment struct {
	ID               int64
	ReviewID         int64
	User             *User
	Body             string
	Path             string
	Line             int
	OriginalLine     int
	Side             string
	CommitID         string
	OriginalCommitID string
	InReplyTo        int64
	Thread           *Thread
	Reactions        []*Reaction
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Thread is a review thread: the comment that opened it and its replies.
type Thread struct {
	ID         int64
	Comments   []*ReviewComment
	Resolved   bool
	Outdated   bool
	ResolvedBy *User
}

// IssueComment is a conversation comment on an issue or PR.
type IssueComment struct {
	ID        int64
	Issue     *Issue
	User      *User
	Body      string
	Reactions []*Reaction
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Reaction is one reaction by one user.
type Reaction struct {
	ID        int64
	User      *User
	Content   string // +1 -1 laugh confused heart hooray rocket eyes
	CreatedAt time.Time
}

// Status is one commit status.
type Status struct {
	ID          int64
	SHA         string
	State       string // error | failure | pending | success
	Context     string
	Description string
	TargetURL   string
	Creator     *User
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CheckSuite groups an app's check runs for one head commit.
type CheckSuite struct {
	ID         int64
	HeadSHA    string
	HeadBranch string
	Status     string
	Conclusion string
	CreatedAt  time.Time
}

// CheckRun is one check (for Actions: one job's check).
type CheckRun struct {
	ID          int64
	SuiteID     int64
	Name        string
	HeadSHA     string
	Status      string // queued | in_progress | completed
	Conclusion  string // success | failure | cancelled | timed_out | … ("" until completed)
	RunID       int64  // the Actions workflow run, 0 for a non-Actions check
	JobID       int64
	StartedAt   time.Time
	CompletedAt time.Time
}

// WorkflowRun is one Actions run.
type WorkflowRun struct {
	ID           int64
	Name         string
	WorkflowID   int64
	SuiteID      int64
	HeadSHA      string
	HeadBranch   string
	Event        string
	Actor        *User
	Status       string
	Conclusion   string
	RunAttempt   int
	RunNumber    int
	CreatedAt    time.Time
	RunStartedAt time.Time
	UpdatedAt    time.Time
}

// Job is one Actions job (its check run is CheckRunID).
type Job struct {
	ID         int64
	RunID      int64
	Name       string
	Status     string
	Conclusion string
	CheckRunID int64
	StartedAt  time.Time
}

// Release is a published release.
type Release struct {
	ID         int64
	TagName    string
	Name       string
	Body       string
	Target     string
	Draft      bool
	Prerelease bool
	Author     *User
	CreatedAt  time.Time
}

func (f *Fake) id() int64 {
	f.seq++
	return f.seq
}

// nodeID is a GitHub-style global node id: a type prefix and an opaque
// base64 body ("PR_kwDOA…"), stable for (kind, id).
func nodeID(prefix string, id int64) string {
	return prefix + "_" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%d", prefix, id)))
}

// parseNodeID is nodeID's inverse.
func parseNodeID(n string) (prefix string, id int64, ok bool) {
	p, body, found := strings.Cut(n, "_")
	if !found {
		return "", 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", 0, false
	}
	kind, num, found := strings.Cut(string(raw), ":")
	if !found || kind != p {
		return "", 0, false
	}
	if _, err := fmt.Sscan(num, &id); err != nil {
		return "", 0, false
	}
	return p, id, true
}

// sha mints a deterministic 40-hex commit sha.
func (f *Fake) sha(seed string) string {
	f.shaSeq++
	sum := sha1.Sum([]byte(fmt.Sprintf("%s#%d", seed, f.shaSeq)))
	return hex.EncodeToString(sum[:])
}

func lower(s string) string { return strings.ToLower(s) }

func (f *Fake) user(login string) *User { return f.users[lower(login)] }

func (f *Fake) repo(full string) *Repo { return f.repos[lower(full)] }

func (r *Repo) pull(n int) *Pull {
	if is := r.Issues[n]; is != nil {
		return is.Pull
	}
	return nil
}

// latestReviews is each reviewer's latest review that counts toward the
// review decision (COMMENTED and PENDING reviews do not), by lowercase login.
func (p *Pull) latestReviews() map[string]*Review {
	out := map[string]*Review{}
	for _, r := range p.Reviews {
		if r.State == "COMMENTED" || r.State == "PENDING" {
			continue
		}
		out[lower(r.User.Login)] = r
	}
	return out
}

// reviewDecision is GitHub's PR review decision: CHANGES_REQUESTED if any
// reviewer's latest opinionated review requests changes; APPROVED once the
// required approvals are met; REVIEW_REQUIRED while a required approval is
// missing; "" (null) when no review policy applies and nobody has reviewed.
func (f *Fake) reviewDecision(r *Repo, p *Pull) string {
	approvals := 0
	for login, rv := range p.latestReviews() {
		if login == lower(p.Issue.User.Login) {
			continue
		}
		switch rv.State {
		case "CHANGES_REQUESTED":
			return "CHANGES_REQUESTED"
		case "APPROVED":
			approvals++
		}
	}
	need := r.Protection.RequiredApprovals
	switch {
	case need > 0 && approvals >= need:
		return "APPROVED"
	case need > 0:
		return "REVIEW_REQUIRED"
	case approvals > 0:
		return "APPROVED"
	}
	return ""
}

// checkStates returns, for the PR head, each check name / status context and
// whether it is passing (success / neutral / skipped), failing, or pending.
// A check re-run supersedes the earlier run of the same name.
func (f *Fake) checkStates(r *Repo, sha string) map[string]string {
	out := map[string]string{}
	for _, c := range r.CheckRuns {
		if c.HeadSHA != sha {
			continue
		}
		switch {
		case c.Status != "completed":
			out[c.Name] = "pending"
		case c.Conclusion == "success" || c.Conclusion == "neutral" || c.Conclusion == "skipped":
			out[c.Name] = "success"
		default:
			out[c.Name] = "failure"
		}
	}
	for _, s := range r.Statuses { // later statuses of a context win
		if s.SHA != sha {
			continue
		}
		switch s.State {
		case "success":
			out[s.Context] = "success"
		case "pending":
			out[s.Context] = "pending"
		default:
			out[s.Context] = "failure"
		}
	}
	return out
}

// mergeState is GitHub's REST mergeable / mergeable_state for a PR, computed
// from the model:
//
//	draft       the PR is a draft
//	dirty       the head conflicts with the base (mergeable false)
//	behind      the base moved past the head's merge base and the base
//	            requires branches to be up to date
//	blocked     a required check is failing or pending, required approvals
//	            are missing, a change is requested, or (when required) a
//	            conversation is unresolved
//	unstable    mergeable, but a non-required check/status is failing or pending
//	clean       none of the above
//
// A closed PR answers null / "unknown"; so does the first read after the
// head or base moved while Options.MergeabilityDelay is on (GitHub computes
// mergeability in the background).
func (f *Fake) mergeState(r *Repo, p *Pull) (mergeable *bool, state string) {
	t, fa := true, false
	if p.Issue.State == "closed" {
		return nil, "unknown"
	}
	if f.opts.MergeabilityDelay && p.mergeReads == 0 {
		p.mergeReads++
		return nil, "unknown"
	}
	p.mergeReads++
	if p.Conflict {
		if p.Draft {
			return &fa, "draft"
		}
		return &fa, "dirty"
	}
	if p.Draft {
		return &t, "draft"
	}
	if r.Protection.RequireUpToDate && r.Branches[p.BaseRef] != "" && r.Branches[p.BaseRef] != p.MergeBase {
		return &t, "behind"
	}
	required := map[string]bool{}
	for _, c := range r.Protection.RequiredChecks {
		required[c] = true
	}
	states := f.checkStates(r, p.HeadSHA)
	blocked := false
	for name := range required {
		if states[name] != "success" {
			blocked = true
		}
	}
	if d := f.reviewDecision(r, p); d == "CHANGES_REQUESTED" || d == "REVIEW_REQUIRED" {
		blocked = true
	}
	if r.Protection.RequireConversationResolution {
		for _, th := range p.Threads {
			if !th.Resolved {
				blocked = true
			}
		}
	}
	if blocked {
		return &t, "blocked"
	}
	for name, st := range states {
		if !required[name] && st != "success" {
			return &t, "unstable"
		}
	}
	return &t, "clean"
}

// combinedState is the combined commit status of sha (statuses only, as the
// REST combined-status endpoint reports it).
func combinedState(statuses []*Status) string {
	if len(statuses) == 0 {
		return "pending"
	}
	state := "success"
	for _, s := range statuses {
		switch s.State {
		case "error", "failure":
			return "failure"
		case "pending":
			state = "pending"
		}
	}
	return state
}

// latestStatuses is the latest status per context for sha, sorted by context.
func latestStatuses(r *Repo, sha string) []*Status {
	by := map[string]*Status{}
	for _, s := range r.Statuses {
		if s.SHA == sha {
			by[s.Context] = s
		}
	}
	out := make([]*Status, 0, len(by))
	for _, s := range by {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Context < out[j].Context })
	return out
}

// touched records that the PR's head or base moved: mergeability is
// recomputed, and inline threads on lines the push touched go outdated.
func (p *Pull) touched() { p.mergeReads = 0 }
