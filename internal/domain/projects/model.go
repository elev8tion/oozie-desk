package projects

import (
	"time"

	"oozie-desk/internal/agent/pi"
)

type User struct {
	ID          int64
	DisplayName string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Organization struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}

type Project struct {
	ID                 int64
	OwnerUserID        int64
	OrganizationID     *int64
	Name               string
	ProjectPathDisplay string
	Trusted            bool
	Archived           bool
	Status             string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type AgentSession struct {
	ID          int64
	ProjectID   int64
	Title       string
	Model       string
	PiSessionID string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type AgentRequest struct {
	ID          int64
	SessionID   int64
	Status      string
	Mode        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

type AgentMessage struct {
	ID           int64
	RequestID    int64
	Role         string
	Status       string
	Content      string
	MetadataJSON string
	Body         string // expandable detail: code written or command output
	CreatedAt    time.Time
}

type PendingQuestion struct {
	ID          int64
	ProjectID   int64
	RequestID   int64
	Prompt      string
	OptionsJSON string
	Options     []string
	Status      string
	RPCID       string
	CreatedAt   time.Time
}

type PermissionRequest struct {
	ID             int64
	ProjectID      int64
	RequestID      int64
	PermissionName string
	Reason         string
	Status         string
	RPCID          string
	CreatedAt      time.Time
}

type PublishDraft struct {
	ProjectID          int64
	AppName            string
	Headline           string
	Description        string
	Changelog          string
	PublishTarget      string
	Visibility         string
	ScreenshotManifest string
	ExpiresDays        int  // 0 = permanent; otherwise the app self-destructs after N days
	AutoInstall        bool // start the local server when the build succeeds
	OrganizationID     *int64
	SavedAt            time.Time
}

type StoreApp struct {
	ID               int64
	ProjectID        *int64
	OrganizationID   *int64
	Name             string
	Headline         string
	Description      string
	Visibility       string
	PublishedVersion string
	LastPublishedAt  *time.Time
	InstallCount     int
	Featured         bool
	Installed        bool
	ArtifactPath     string
	BundleSlug       string
	PublicURL        string
	RuntimePID       int
	ExpiresAt        *time.Time
	LaunchCount      int
	LastLaunchAt     *time.Time
	CreatedAt        time.Time
}

// ExpiresInDays returns whole days until self-destruction (0 = today).
func (s StoreApp) ExpiresInDays() int {
	if s.ExpiresAt == nil {
		return 0
	}
	d := int(time.Until(*s.ExpiresAt).Hours() / 24)
	if d < 0 {
		return 0
	}
	return d
}

// Dormant reports an installed app that hasn't been opened in two weeks
// (or ever, two weeks after publishing).
func (s StoreApp) Dormant() bool {
	if !s.Installed {
		return false
	}
	cutoff := time.Now().Add(-14 * 24 * time.Hour)
	if s.LastLaunchAt != nil {
		return s.LastLaunchAt.Before(cutoff)
	}
	ref := s.CreatedAt
	if s.LastPublishedAt != nil {
		ref = *s.LastPublishedAt
	}
	return ref.Before(cutoff)
}

// ImproveRequest links an agent request filed from inside a published app
// to the store app that should auto-republish when it completes.
type ImproveRequest struct {
	ID         int64
	RequestID  int64
	StoreAppID int64
	Status     string // building|publishing|done|failed
	Note       string
	CreatedAt  time.Time
}

type PublishingJob struct {
	ID           int64
	ProjectID    int64
	ProjectName  string
	StoreAppID   *int64
	Status       string
	ErrorMessage string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Settings struct {
	Appearance   string
	StyleProfile string
	FairyEnabled bool
	FairyHour    int // local hour (0-23) the night shift starts
	// CodingModel is the desk-wide preferred provider/model (e.g. xai/grok-4.3).
	// Empty means the catalog default. Operator can switch anytime in Settings or on the desk.
	CodingModel string
}

// A Wish is an app idea waiting for the nightly build fairy.
type Wish struct {
	ID        int64
	Text      string
	Status    string // pending|building|built|failed
	ProjectID *int64
	Error     string
	CreatedAt time.Time
	BuiltAt   *time.Time
}

// RecipeDraft is a store-link proposal waiting for Accept / Reject / Edit.
// Status: pending | accepted | rejected.
type RecipeDraft struct {
	ID               int64
	SourceURL        string
	SourceKind       string // chrome | appstore | play
	Name             string
	Headline         string
	StoreDescription string
	Plan             string // natural-language "what will be built"
	RecipeJSON       string
	Status           string
	ProjectID        *int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Dashboard struct {
	Projects  []Project
	StoreApps []StoreApp
	Jobs      []PublishingJob
}

type ModelOption struct {
	Provider string
	ID       string
	Full     string
}

type AgentPage struct {
	Project    Project
	Session    AgentSession
	Requests   []AgentRequest
	Messages   []AgentMessage
	Question   *PendingQuestion
	Permission *PermissionRequest
	Mode       string
	Error      string
	Models     []ModelOption
	Model      string
	Streaming  bool
	Stats      *pi.SessionStats
}

type ErrValidation struct{ Message string }

func (e ErrValidation) Error() string { return e.Message }
