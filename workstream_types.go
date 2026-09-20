package main

import (
	"database/sql"
	"errors"
	"sync"
	"time"
)

// WorkstreamStore is the sole domain boundary for Map reads and mutations.
// now and location are injected so HTTP tests and deadline rendering share a
// deterministic notion of time.
type WorkstreamStore struct {
	db            *sql.DB
	now           func() time.Time
	location      *time.Location
	mirrorEnabled bool
	afterCommit   func()
	mu            sync.Mutex
}

func NewWorkstreamStore(db *sql.DB, now func() time.Time, location *time.Location) *WorkstreamStore {
	if now == nil {
		now = time.Now
	}
	if location == nil {
		location = time.Local
	}
	return &WorkstreamStore{db: db, now: now, location: location, mirrorEnabled: true}
}

func (st *WorkstreamStore) SetMirrorEnabled(enabled bool) { st.mirrorEnabled = enabled }
func (st *WorkstreamStore) SetAfterCommit(fn func())      { st.afterCommit = fn }

type Workstream struct {
	ID, Name, Outcome, Owner, Sponsor, TargetDate, Notes, Lifecycle, CompletionNote string
	Archived                                                                        bool
	Revision                                                                        int64
	CreatedAt, UpdatedAt                                                            time.Time
}

type WorkstreamItem struct {
	ID, WorkstreamID, Kind, Title, Description string
	SourceID                                   string
	DueDate, TrackingState, BlockerReason      string
	Counterpart, AskStatus                     string
	FollowUpAt, LastContactAt                  *time.Time
	Value, Unit                                string
	ObservedAt                                 *time.Time
	Assessment                                 string
	ReviewBy                                   *time.Time
	DetachedAt                                 *time.Time
	Revision                                   int64
	CreatedAt, UpdatedAt                       time.Time
}

type Source struct {
	ID, Kind, URL, CanonicalID, Label        string
	PRID                                     *int64
	CachedTitle, CachedState, LocalReviewURL string
	LastObservedAt                           *time.Time
	Revision                                 int64
	CreatedAt, UpdatedAt                     time.Time
}

type Decision struct {
	ID, WorkstreamID, ItemID, SourceID, Note string
	Revision                                 int64
	CreatedAt                                time.Time
}

type TimelineEventView struct {
	ID, WorkstreamID, EntityType, EntityID, EventType, Description string
	Revision                                                       int64
	CreatedAt                                                      time.Time
}

type MirrorKey struct{ EntityType, EntityID string }
type MirrorStateView struct {
	MirrorKey
	Status, LastChecksum, Error                               string
	DesiredRevision, LastWrittenRevision, LastSuccessRevision int64
	LastAttemptAt, LastSuccessAt                              *time.Time
}

// MirrorDocument is data only. The mirror worker owns Markdown formatting and
// filesystem work; it never needs to query domain tables.
type MirrorDocument struct {
	Key          MirrorKey
	RelativePath string
	Revision     int64
	Frontmatter  map[string]string
	Title, Body  string
	Links        []MirrorLink
}
type MirrorLink struct{ Label, RelativePath string }

var ErrRevisionUnavailable = errors.New("mirror revision unavailable")

type WorkstreamInput struct {
	Name       string `json:"name"`
	Outcome    string `json:"outcome"`
	Owner      string `json:"owner"`
	Sponsor    string `json:"sponsor"`
	TargetDate string `json:"target_date"`
	Notes      string `json:"notes"`
}
type WorkstreamItemInput struct {
	Kind          string `json:"kind"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	SourceURL     string `json:"source_url"`
	SourceLabel   string `json:"source_label"`
	SourceKind    string `json:"source_kind"`
	PRID          int64  `json:"pr_id"`
	DueDate       string `json:"due_date"`
	TrackingState string `json:"tracking_state"`
	BlockerReason string `json:"blocker_reason"`
	Counterpart   string `json:"counterpart"`
	AskStatus     string `json:"ask_status"`
	FollowUpAt    string `json:"follow_up_at"`
	LastContactAt string `json:"last_contact_at"`
	Value         string `json:"value"`
	Unit          string `json:"unit"`
	ObservedAt    string `json:"observed_at"`
	Assessment    string `json:"assessment"`
	ReviewBy      string `json:"review_by"`
}
type DecisionInput struct {
	Note     string `json:"note"`
	ItemID   string `json:"item_id"`
	SourceID string `json:"source_id"`
}

// WorkstreamCommand is the stable command body for forms and /map/commands.
// Action is one of create_workstream, edit_workstream, complete_workstream,
// reopen_workstream, archive_workstream, restore_workstream, create_item,
// edit_item, detach_item, restore_item, move_item, or record_decision.
type WorkstreamCommand struct {
	OperationID             string              `json:"operation_id"`
	Action                  string              `json:"action"`
	WorkstreamID            string              `json:"workstream_id"`
	ItemID                  string              `json:"item_id"`
	DestinationWorkstreamID string              `json:"destination_workstream_id"`
	ExpectedRevision        int64               `json:"expected_revision"`
	Confirm                 bool                `json:"confirm"`
	OverrideReason          string              `json:"override_reason"`
	Workstream              WorkstreamInput     `json:"workstream"`
	Item                    WorkstreamItemInput `json:"item"`
	Decision                DecisionInput       `json:"decision"`
}
type CommandResult struct {
	OperationID, WorkstreamID, ItemID, DecisionID string
	Revision                                      int64
	Idempotent                                    bool
}

type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

type ConflictError struct {
	EntityID                         string
	ExpectedRevision, ActualRevision int64
}

func (e *ConflictError) Error() string { return "stale revision" }

type NotFoundError struct{ EntityType, EntityID string }

func (e *NotFoundError) Error() string { return "not found" }

type Page[T any] struct {
	PreviousCursor string
	Rows           []T
	NextCursor     string
	Total          int
}
type MapQuery struct {
	UnfinishedCursor                                              string
	ItemsCategory, DetachedCursor, DecisionsCursor, ReasonsCursor string
	WorkstreamID, ItemID, Tab, Filter, Search                     string
	WorkstreamsCursor, ItemsCursor, TimelineCursor                string
	WorkstreamsLimit, ItemsLimit, TimelineLimit                   int
}
type AttentionReason struct{ EntityID, EntityType, Category, Code, Label string }
type WorkstreamListRow struct {
	Workstream
	Attention        string
	AttentionCount   int
	EarliestDeadline string
}
type WorkstreamItemView struct {
	WorkstreamItem
	Source    *Source
	Attention []AttentionReason
	Mirror    MirrorStateView
}
type DecisionView struct {
	Decision
	ItemWorkstreamID string
	Mirror           MirrorStateView
}
type CategoryView struct {
	Key, Label, Attention string
	AttentionCount, Total int
	Items                 Page[WorkstreamItemView]
}
type WorkstreamDetailView struct {
	AttentionCount, ReasonsTotal, DecisionsTotal int
	ReasonsNextCursor, DecisionsNextCursor       string
	Workstream
	Attention                         string
	AttentionReasons                  []AttentionReason
	Warnings                          []string
	OpenTaskCount, UnresolvedAskCount int
	Mirror                            MirrorStateView
	Decisions                         []DecisionView
}
type GraphNode struct {
	ID, Label, Kind, ParentID string
	Omitted                   int
}
type GraphView struct {
	Nodes   []GraphNode
	Omitted int
}
type MapPageView struct {
	Unfinished            Page[WorkstreamItemView]
	Query                 MapQuery
	Workstreams           Page[WorkstreamListRow]
	Selected              *WorkstreamDetailView
	SelectedItem          *WorkstreamItemView
	Categories            []CategoryView
	Timeline              Page[TimelineEventView]
	Graph                 GraphView
	Mirror                MirrorSummaryView
	Detached              Page[WorkstreamItemView]
	ActiveWorkstreamCount int
	NotFound              string
}
type MirrorSummaryView struct {
	Disabled                   bool
	Pending, Errors, Conflicts int
	States                     []MirrorStateView
}
type PRSearchQuery struct {
	Search, Cursor string
	Limit          int
}
type CachedPRView struct {
	ID                int64
	Owner, Repo       string
	Number            int
	URL, Title, State string
	LastObservedAt    *time.Time
}
type PRSearchPage struct {
	Rows       []CachedPRView
	NextCursor string
	Total      int
}
