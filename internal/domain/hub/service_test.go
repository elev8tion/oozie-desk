package hub

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"oozie"
	"oozie/internal/db"
	"oozie/internal/domain/projects"
)

func TestInviteShareAndRevoke(t *testing.T) {
	ctx := context.Background()
	authorDB, author := openDesk(t)
	_, guest := openDesk(t)
	author.allowLoopback = true
	guest.allowLoopback = true
	t.Cleanup(author.Stop)
	t.Cleanup(guest.Stop)

	if err := validateAddr("8.8.8.8:8091", false); err == nil {
		t.Fatal("public address was accepted")
	}
	if err := validateAddr("127.0.0.1:8091", false); err == nil {
		t.Fatal("loopback was accepted outside a test")
	}

	invite, err := author.CreateInvite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := guest.AcceptInvite(ctx, invite); err != nil {
		t.Fatal(err)
	}
	if err := guest.AcceptInvite(ctx, invite); err == nil {
		t.Fatal("a used invitation was accepted again")
	}

	appID := seedTool(t, authorDB)
	peers, err := author.Peers(ctx)
	if err != nil || len(peers) != 1 {
		t.Fatalf("author peers = %+v, %v", peers, err)
	}
	if err := author.ActivateShare(ctx, appID, peers[0].ID); err != nil {
		t.Fatal(err)
	}
	inbox, err := guest.Inbox(ctx)
	if err != nil || len(inbox) != 1 {
		t.Fatalf("inbox = %+v, %v", inbox, err)
	}
	raw, err := guest.fetchShare(ctx, Peer{NodeID: author.NodeID(), Addr: author.Addr()}, inbox[0].Token)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "sourdough") || strings.Contains(raw, "icon_png") {
		t.Fatalf("share payload = %s", raw)
	}
	// Share is recipe JSON only — never store runtime metadata or a binary path.
	if strings.Contains(raw, "artifact") || strings.Contains(raw, "runtime_pid") || strings.Contains(raw, "public_url") {
		t.Fatalf("share payload looked like store metadata: %s", raw)
	}
	if !strings.Contains(raw, "oozie-recipe/v1") || !strings.Contains(raw, "\"prompts\"") {
		t.Fatalf("share payload missing recipe shape: %s", raw)
	}
	if err := author.StopShare(ctx, appID, peers[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := guest.fetchShare(ctx, Peer{NodeID: author.NodeID(), Addr: author.Addr()}, inbox[0].Token); err == nil {
		t.Fatal("stopped link still returned a recipe")
	}
	if err := author.Revoke(ctx, peers[0].ID); err != nil {
		t.Fatal(err)
	}
	left, _ := author.Peers(ctx)
	if len(left) != 0 {
		t.Fatalf("revoked peer still listed: %+v", left)
	}
}

func openDesk(t *testing.T) (*sql.DB, *Service) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "desk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	migrations, err := fs.Sub(oozie.Assets, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RunMigrations(database, migrations); err != nil {
		t.Fatal(err)
	}
	return database, NewService(NewRepo(database), projects.NewService(projects.NewRepo(database)))
}

func seedTool(t *testing.T, database *sql.DB) int64 {
	t.Helper()
	res, err := database.Exec(`INSERT INTO projects (owner_user_id, organization_id, name, project_path_display, trusted, archived, status) VALUES (1,1,'Tool','/tmp/tool',1,0,'ready')`)
	if err != nil {
		t.Fatal(err)
	}
	projectID, _ := res.LastInsertId()
	if _, err := database.Exec(`INSERT INTO agent_sessions (project_id, title) VALUES (?, 'build')`, projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO agent_requests (session_id, status, mode) VALUES (1, 'completed', 'build')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO agent_messages (request_id, role, status, content) VALUES (1, 'user', 'completed', 'A sourdough page')`); err != nil {
		t.Fatal(err)
	}
	res, err = database.Exec(`INSERT INTO store_apps (project_id, organization_id, name, headline, description, visibility) VALUES (?, 1, 'Tool', 'starter', 'tracks feedings', 'unlisted')`, projectID)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}
