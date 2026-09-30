package hub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type Repo struct {
	db *sql.DB
}

func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

func (r *Repo) EnsureIdentity(ctx context.Context) (ed25519.PrivateKey, string, error) {
	var privHex, nodeID string
	err := r.db.QueryRowContext(ctx, `SELECT private_key, node_id FROM node_keys WHERE id=1`).Scan(&privHex, &nodeID)
	if err == nil {
		priv, err := hex.DecodeString(privHex)
		if err != nil || len(priv) != ed25519.PrivateKeySize {
			return nil, "", fmt.Errorf("stored desk key is invalid")
		}
		return ed25519.PrivateKey(priv), nodeID, nil
	}
	if err != sql.ErrNoRows {
		return nil, "", err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	nodeID = hex.EncodeToString(pub)
	_, err = r.db.ExecContext(ctx, `INSERT INTO node_keys (id, node_id, private_key) VALUES (1, ?, ?)`, nodeID, hex.EncodeToString(priv))
	if err != nil {
		return nil, "", err
	}
	return priv, nodeID, nil
}

func (r *Repo) Identity(ctx context.Context, nodeID, addr string) (Identity, error) {
	var id Identity
	var connect int
	err := r.db.QueryRowContext(ctx, `
		SELECT u.display_name, o.name, o.industry_pack, s.connect_enabled
		FROM users u, organizations o, user_settings s
		WHERE u.id = (SELECT id FROM users ORDER BY id LIMIT 1)
		  AND o.id = (SELECT id FROM organizations ORDER BY id LIMIT 1)
		  AND s.user_id = u.id`).Scan(&id.DisplayName, &id.CircleName, &id.IndustryPack, &connect)
	if err != nil {
		return Identity{}, err
	}
	id.NodeID = nodeID
	id.Connect = connect == 1
	id.Addr = addr
	id.IndustryPack = strings.TrimSpace(id.IndustryPack)
	id.FirstName, id.LastInitial = SplitDeskName(id.DisplayName)
	return id, nil
}

func (r *Repo) SaveIdentity(ctx context.Context, name, circle string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET display_name=?, updated_at=CURRENT_TIMESTAMP WHERE id=(SELECT id FROM users ORDER BY id LIMIT 1)`, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE organizations SET name=? WHERE id=(SELECT id FROM organizations ORDER BY id LIMIT 1)`, circle); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repo) SetConnect(ctx context.Context, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := r.db.ExecContext(ctx, `UPDATE user_settings SET connect_enabled=?, updated_at=CURRENT_TIMESTAMP WHERE user_id=(SELECT id FROM users ORDER BY id LIMIT 1)`, v)
	return err
}

func (r *Repo) ConnectEnabled(ctx context.Context) bool {
	var v int
	_ = r.db.QueryRowContext(ctx, `SELECT connect_enabled FROM user_settings WHERE user_id=(SELECT id FROM users ORDER BY id LIMIT 1)`).Scan(&v)
	return v == 1
}

func (r *Repo) CreateInvite(ctx context.Context, code string, expires time.Time) error {
	sum := sha256.Sum256([]byte(code))
	_, err := r.db.ExecContext(ctx, `INSERT INTO invitations (code_hash, expires_at) VALUES (?, ?)`, hex.EncodeToString(sum[:]), expires.Unix())
	return err
}

// ConsumeInvite marks a code used. A second use, or an expired code, fails.
func (r *Repo) ConsumeInvite(ctx context.Context, code string, now time.Time) error {
	sum := sha256.Sum256([]byte(code))
	res, err := r.db.ExecContext(ctx, `UPDATE invitations SET used_at=? WHERE code_hash=? AND used_at IS NULL AND expires_at > ?`, now.Unix(), hex.EncodeToString(sum[:]), now.Unix())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("invitation is expired or already used")
	}
	return nil
}

func (r *Repo) UpsertPeer(ctx context.Context, p Peer) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO peers (node_id, display_name, circle_name, addr, revoked, last_seen)
		VALUES (?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
		ON CONFLICT(node_id) DO UPDATE SET
			display_name=excluded.display_name,
			circle_name=excluded.circle_name,
			addr=excluded.addr,
			revoked=0,
			last_seen=CURRENT_TIMESTAMP`, p.NodeID, p.DisplayName, p.CircleName, p.Addr)
	if err != nil {
		return 0, err
	}
	var id int64
	err = r.db.QueryRowContext(ctx, `SELECT id FROM peers WHERE node_id=?`, p.NodeID).Scan(&id)
	if err != nil {
		return res.LastInsertId()
	}
	return id, nil
}

func (r *Repo) ListPeers(ctx context.Context) ([]Peer, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, node_id, display_name, circle_name, addr FROM peers WHERE revoked=0 ORDER BY display_name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Peer
	for rows.Next() {
		var p Peer
		if err := rows.Scan(&p.ID, &p.NodeID, &p.DisplayName, &p.CircleName, &p.Addr); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repo) PeerByID(ctx context.Context, id int64) (Peer, error) {
	var p Peer
	err := r.db.QueryRowContext(ctx, `SELECT id, node_id, display_name, circle_name, addr FROM peers WHERE id=? AND revoked=0`, id).Scan(&p.ID, &p.NodeID, &p.DisplayName, &p.CircleName, &p.Addr)
	return p, err
}

func (r *Repo) PeerByNode(ctx context.Context, nodeID string) (Peer, error) {
	var p Peer
	err := r.db.QueryRowContext(ctx, `SELECT id, node_id, display_name, circle_name, addr FROM peers WHERE node_id=? AND revoked=0`, nodeID).Scan(&p.ID, &p.NodeID, &p.DisplayName, &p.CircleName, &p.Addr)
	return p, err
}

func (r *Repo) RevokePeer(ctx context.Context, id int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM share_grants WHERE peer_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE peers SET revoked=1, addr='' WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repo) PutGrant(ctx context.Context, appID, peerID int64, token string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM share_grants WHERE store_app_id=? AND peer_id=?`, appID, peerID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO share_grants (store_app_id, peer_id, token) VALUES (?, ?, ?)`, appID, peerID, token); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repo) DeleteGrant(ctx context.Context, appID, peerID int64) error {
	q := `DELETE FROM share_grants WHERE store_app_id=?`
	args := []any{appID}
	if peerID > 0 {
		q += ` AND peer_id=?`
		args = append(args, peerID)
	}
	_, err := r.db.ExecContext(ctx, q, args...)
	return err
}

func (r *Repo) ListGrants(ctx context.Context) ([]Grant, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT g.store_app_id, g.peer_id, p.display_name, g.token
		FROM share_grants g JOIN peers p ON p.id = g.peer_id
		WHERE p.revoked=0
		ORDER BY g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.AppID, &g.PeerID, &g.PeerName, &g.Token); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (r *Repo) GrantPeer(ctx context.Context, token string) (int64, string, error) {
	var peerID int64
	var nodeID string
	err := r.db.QueryRowContext(ctx, `
		SELECT g.peer_id, p.node_id
		FROM share_grants g JOIN peers p ON p.id = g.peer_id
		WHERE g.token=? AND p.revoked=0`, token).Scan(&peerID, &nodeID)
	return peerID, nodeID, err
}

func (r *Repo) PutInbox(ctx context.Context, item InboxItem) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO share_inbox (author_node_id, author_name, token, app_name, headline, status)
		VALUES (?, ?, ?, ?, ?, 'pending')
		ON CONFLICT(author_node_id, token) DO UPDATE SET
			author_name=excluded.author_name,
			app_name=excluded.app_name,
			headline=excluded.headline,
			status='pending'`, item.AuthorNode, item.AuthorName, item.Token, item.AppName, item.Headline)
	return err
}

func (r *Repo) ListInbox(ctx context.Context) ([]InboxItem, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, author_node_id, author_name, token, app_name, headline, status FROM share_inbox WHERE status='pending' ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxItem
	for rows.Next() {
		var item InboxItem
		if err := rows.Scan(&item.ID, &item.AuthorNode, &item.AuthorName, &item.Token, &item.AppName, &item.Headline, &item.Status); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *Repo) Inbox(ctx context.Context, id int64) (InboxItem, error) {
	var item InboxItem
	err := r.db.QueryRowContext(ctx, `SELECT id, author_node_id, author_name, token, app_name, headline, status FROM share_inbox WHERE id=?`, id).Scan(&item.ID, &item.AuthorNode, &item.AuthorName, &item.Token, &item.AppName, &item.Headline, &item.Status)
	return item, err
}

func (r *Repo) SetInboxStatus(ctx context.Context, id int64, status string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE share_inbox SET status=? WHERE id=?`, status, id)
	return err
}
