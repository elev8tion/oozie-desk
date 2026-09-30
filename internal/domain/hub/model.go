package hub

import "time"

// Identity is this desk. One operator, one circle, no remote account.
type Identity struct {
	DisplayName  string
	FirstName    string
	LastInitial  string
	CircleName   string
	IndustryPack string
	NodeID       string
	Connect      bool
	Addr         string
}

// Peer is an invited desk. Addr is its private hub listener, not a public URL.
type Peer struct {
	ID          int64
	NodeID      string
	DisplayName string
	CircleName  string
	Addr        string
	LastSeen    *time.Time
}

// Grant is an activated share link for one peer and one tool.
type Grant struct {
	AppID    int64
	PeerID   int64
	PeerName string
	Token    string
	Link     string
}

// InboxItem is a share notice pushed by a paired desk. The recipe is fetched
// only when the operator accepts it.
type InboxItem struct {
	ID         int64
	AuthorNode string
	AuthorName string
	Token      string
	AppName    string
	Headline   string
	Status     string
}

// Sidebar is the status pinned under the nav. It is this desk, not a slogan.
type Sidebar struct {
	Name    string
	Circle  string
	Addr    string
	Connect bool
	Running int
	People  int
	Tools   []SidebarTool
}

// SidebarTool is a tool this desk is serving right now.
type SidebarTool struct {
	Name string
	URL  string
}

// Desk is the front door: make a tool, see what is running, see what was shared.
type Desk struct {
	Text     string
	Err      string
	Flash    string
	Setup    string // non-empty when the desk cannot build yet (no signed-in model)
	Apps     any
	Inbox    []InboxItem
	Peers    []Peer
	Grants   []Grant
	Insights any
	Connect  bool
	Addr     string
}
