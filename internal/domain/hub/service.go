package hub

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"oozie-desk/internal/domain/projects"
)

const inviteTTL = 15 * time.Minute

type Service struct {
	repo          *Repo
	projects      *projects.Service
	allowLoopback bool

	mu     sync.Mutex
	key    ed25519.PrivateKey
	nodeID string
	srv    *http.Server
	addr   string
}

func NewService(repo *Repo, projectsSvc *projects.Service) *Service {
	s := &Service{repo: repo, projects: projectsSvc}
	key, nodeID, err := repo.EnsureIdentity(context.Background())
	if err == nil {
		s.key = key
		s.nodeID = nodeID
	}
	return s
}

// Restore opens the private listener again when this desk left Connect on.
func (s *Service) Restore(ctx context.Context) {
	if s.repo.ConnectEnabled(ctx) {
		if _, err := s.Connect(ctx); err != nil {
			_ = s.repo.SetConnect(ctx, false)
		}
	}
}

// Stop closes the private listener and leaves the Connect switch as it is.
func (s *Service) Stop() {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.addr = ""
	s.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

func (s *Service) Disconnect() {
	s.Stop()
	_ = s.repo.SetConnect(context.Background(), false)
}

func (s *Service) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

func (s *Service) NodeID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nodeID
}

func (s *Service) Identity(ctx context.Context) (Identity, error) {
	return s.repo.Identity(ctx, s.NodeID(), s.Addr())
}

func (s *Service) SaveIdentity(ctx context.Context, first, initial, circle string) error {
	name, err := FormatDeskName(first, initial)
	if err != nil {
		return err
	}
	circle = strings.TrimSpace(circle)
	if len(circle) > 80 {
		return errors.New("Name and circle must be 80 characters or fewer.")
	}
	if circle == "" {
		circle = "Personal Workspace"
	}
	return s.repo.SaveIdentity(ctx, name, circle)
}

// Connect binds a second listener on a private address. The factory UI stays
// on loopback. Loopback pairing is refused unless a test opted in.
func (s *Service) Connect(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return s.addr, nil
	}
	if s.key == nil {
		return "", errors.New("This desk has no identity key.")
	}
	host, err := s.bindHost()
	if err != nil {
		return "", err
	}
	cert, err := s.certificate(net.ParseIP(host))
	if err != nil {
		return "", err
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequestClientCert,
		MinVersion:   tls.VersionTLS13,
	}
	var ln net.Listener
	var addr string
	for port := 8091; port < 8111; port++ {
		addr = net.JoinHostPort(host, strconv.Itoa(port))
		ln, err = tls.Listen("tcp", addr, cfg)
		if err == nil {
			break
		}
	}
	if ln == nil {
		return "", errors.New("Could not open a private port for invitations.")
	}
	srv := &http.Server{
		Handler:           s.hubMux(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
	}
	s.srv = srv
	s.addr = addr
	go func() { _ = srv.Serve(ln) }()
	if err := s.repo.SetConnect(ctx, true); err != nil {
		return addr, err
	}
	return addr, nil
}

func (s *Service) bindHost() (string, error) {
	if s.allowLoopback {
		return "127.0.0.1", nil
	}
	ip, err := privateIPv4()
	if err != nil {
		return "", errors.New("No private network address. This desk stays local-only.")
	}
	return ip.String(), nil
}

func (s *Service) CreateInvite(ctx context.Context) (string, error) {
	addr, err := s.Connect(ctx)
	if err != nil {
		return "", err
	}
	code, err := newCode()
	if err != nil {
		return "", err
	}
	if err := s.repo.CreateInvite(ctx, code, time.Now().Add(inviteTTL)); err != nil {
		return "", err
	}
	return code + "@" + addr, nil
}

func (s *Service) AcceptInvite(ctx context.Context, raw string) error {
	code, addr, err := splitInvite(raw)
	if err != nil {
		return err
	}
	if err := validateAddr(addr, s.allowLoopback); err != nil {
		return err
	}
	if _, err := s.Connect(ctx); err != nil {
		return err
	}
	ident, err := s.Identity(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{
		"code":         code,
		"display_name": ident.DisplayName,
		"circle_name":  ident.CircleName,
		"addr":         s.Addr(),
	})
	var resp struct {
		NodeID      string `json:"node_id"`
		DisplayName string `json:"display_name"`
		CircleName  string `json:"circle_name"`
		Addr        string `json:"addr"`
	}
	seen, err := s.postJSON(ctx, addr, "", "/hub/v1/accept", body, &resp)
	if err != nil {
		return err
	}
	if seen == "" || seen != resp.NodeID {
		return errors.New("That desk's key did not match its certificate.")
	}
	if seen == s.NodeID() {
		return errors.New("You cannot invite this same desk.")
	}
	_, err = s.repo.UpsertPeer(ctx, Peer{NodeID: resp.NodeID, DisplayName: cleanName(resp.DisplayName), CircleName: cleanName(resp.CircleName), Addr: resp.Addr})
	return err
}

func (s *Service) Revoke(ctx context.Context, id int64) error {
	return s.repo.RevokePeer(ctx, id)
}

func (s *Service) Peers(ctx context.Context) ([]Peer, error) {
	return s.repo.ListPeers(ctx)
}

// ActivateShare turns a link on for one invited peer, or every peer when
// peerID is 0. The payload is a recipe (prompts + design). The author's
// tool data/ directory, databases, and usage records are never sent.
func (s *Service) ActivateShare(ctx context.Context, appID, peerID int64) error {
	if s.Addr() == "" {
		if _, err := s.Connect(ctx); err != nil {
			return err
		}
	}
	app, err := s.projects.GetStoreApp(ctx, appID)
	if err != nil {
		return errors.New("That tool is not on this desk.")
	}
	if _, err := s.projects.ExportRecipe(ctx, appID); err != nil {
		return errors.New("Build the tool once before sharing it. A share sends the recipe, and this one has no prompts yet.")
	}
	peers, err := s.repo.ListPeers(ctx)
	if err != nil {
		return err
	}
	var chosen []Peer
	for _, p := range peers {
		if peerID == 0 || p.ID == peerID {
			chosen = append(chosen, p)
		}
	}
	if len(chosen) == 0 {
		return errors.New("Invite someone from People before sharing.")
	}
	ident, _ := s.Identity(ctx)
	for _, p := range chosen {
		token, err := newToken()
		if err != nil {
			return err
		}
		if err := s.repo.PutGrant(ctx, appID, p.ID, token); err != nil {
			return err
		}
		note, _ := json.Marshal(map[string]string{
			"token":    token,
			"app_name": app.Name,
			"headline": app.Headline,
			"author":   ident.DisplayName,
		})
		_, _ = s.postJSON(ctx, p.Addr, p.NodeID, "/hub/v1/inbox", note, nil)
	}
	return nil
}

func (s *Service) StopShare(ctx context.Context, appID, peerID int64) error {
	return s.repo.DeleteGrant(ctx, appID, peerID)
}

func (s *Service) Grants(ctx context.Context) ([]Grant, error) {
	grants, err := s.repo.ListGrants(ctx)
	if err != nil {
		return nil, err
	}
	node := s.NodeID()
	for i := range grants {
		grants[i].Link = linkFor(node, grants[i].Token)
	}
	return grants, nil
}

func (s *Service) Inbox(ctx context.Context) ([]InboxItem, error) {
	return s.repo.ListInbox(ctx)
}

// AcceptInbox fetches the recipe from the author's desk and imports it here.
func (s *Service) AcceptInbox(ctx context.Context, id int64) (projects.Project, error) {
	item, err := s.repo.Inbox(ctx, id)
	if err != nil {
		return projects.Project{}, errors.New("That share is no longer waiting.")
	}
	project, err := s.acceptToken(ctx, item.AuthorNode, item.Token)
	if err != nil {
		return projects.Project{}, err
	}
	_ = s.repo.SetInboxStatus(ctx, id, "accepted")
	return project, nil
}

func (s *Service) DismissInbox(ctx context.Context, id int64) error {
	return s.repo.SetInboxStatus(ctx, id, "dismissed")
}

func (s *Service) AcceptLink(ctx context.Context, link string) (projects.Project, error) {
	nodeID, token, err := parseLink(link)
	if err != nil {
		return projects.Project{}, err
	}
	return s.acceptToken(ctx, nodeID, token)
}

func (s *Service) acceptToken(ctx context.Context, nodeID, token string) (projects.Project, error) {
	peer, err := s.repo.PeerByNode(ctx, nodeID)
	if err != nil {
		return projects.Project{}, errors.New("That desk is not invited here.")
	}
	raw, err := s.fetchShare(ctx, peer, token)
	if err != nil {
		return projects.Project{}, err
	}
	return s.projects.ImportRecipe(ctx, raw)
}

func (s *Service) fetchShare(ctx context.Context, peer Peer, token string) (string, error) {
	if err := validateAddr(peer.Addr, s.allowLoopback); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	_, err := s.doJSON(ctx, peer.Addr, peer.NodeID, http.MethodGet, "/hub/v1/shares/"+token, nil, &buf)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

func linkFor(nodeID, token string) string {
	return "share:" + nodeID + ":" + token
}

func parseLink(link string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(link), ":")
	if len(parts) != 3 || parts[0] != "share" || parts[1] == "" || parts[2] == "" {
		return "", "", errors.New("A share link looks like share:<desk>:<token>.")
	}
	return parts[1], parts[2], nil
}

func splitInvite(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	i := strings.LastIndex(raw, "@")
	if i <= 0 || i == len(raw)-1 {
		return "", "", errors.New("Paste the invitation as code@address.")
	}
	return raw[:i], raw[i+1:], nil
}

func newCode() (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, 8)
	for i := range out {
		out[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(out[:4]) + "-" + string(out[4:]), nil
}

func newToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func cleanName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Desk"
	}
	if len(s) > 80 {
		return s[:80]
	}
	return s
}

type dialResult struct {
	nodeID string
}

func (s *Service) postJSON(ctx context.Context, addr, pin, path string, body []byte, dest any) (string, error) {
	var buf bytes.Buffer
	seen, err := s.doJSON(ctx, addr, pin, http.MethodPost, path, body, &buf)
	if err != nil {
		return seen, err
	}
	if dest != nil && buf.Len() > 0 {
		if err := json.Unmarshal(buf.Bytes(), dest); err != nil {
			return seen, errors.New("That desk sent a response this one cannot read.")
		}
	}
	return seen, nil
}

func (s *Service) doJSON(ctx context.Context, addr, pin, method, path string, body []byte, dest *bytes.Buffer) (string, error) {
	if err := validateAddr(addr, s.allowLoopback); err != nil {
		return "", err
	}
	seen := &dialResult{}
	client, err := s.client(pin, seen)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+addr+path, reader)
	if err != nil {
		return "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if addr := s.Addr(); addr != "" {
		req.Header.Set("X-Oozie-Addr", addr)
	}
	res, err := client.Do(req)
	if err != nil {
		return "", errors.New("Could not reach that desk on the private network.")
	}
	defer res.Body.Close()
	limited := io.LimitReader(res.Body, 2<<20)
	payload, _ := io.ReadAll(limited)
	if res.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(payload))
		if msg == "" || len(msg) > 180 {
			msg = "That desk refused the request."
		}
		return seen.nodeID, errors.New(msg)
	}
	if dest != nil {
		dest.Write(payload)
	}
	return seen.nodeID, nil
}

func (s *Service) client(pin string, seen *dialResult) (*http.Client, error) {
	s.mu.Lock()
	key := s.key
	s.mu.Unlock()
	if key == nil {
		return nil, errors.New("desk key is not ready")
	}
	cert, err := s.certificate(net.ParseIP("127.0.0.1"))
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{cert},
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("missing desk certificate")
			}
			parsed, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			id, err := nodeIDFromCert(parsed)
			if err != nil {
				return err
			}
			if pin != "" && id != pin {
				return errors.New("desk key changed")
			}
			seen.nodeID = id
			return nil
		},
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}, nil
}

func (s *Service) hubMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hub/v1/accept", s.handleAccept)
	mux.HandleFunc("POST /hub/v1/inbox", s.handleInbox)
	mux.HandleFunc("GET /hub/v1/shares/{token}", s.handleShare)
	return mux
}

func (s *Service) guard(w http.ResponseWriter, r *http.Request) bool {
	if err := remoteAllowed(r.RemoteAddr, s.allowLoopback); err != nil {
		http.Error(w, "private network only", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Service) peerCert(r *http.Request) (string, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", errors.New("missing desk certificate")
	}
	return nodeIDFromCert(r.TLS.PeerCertificates[0])
}

func (s *Service) handleAccept(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	clientID, err := s.peerCert(r)
	if err != nil {
		http.Error(w, "missing desk certificate", http.StatusUnauthorized)
		return
	}
	if clientID == s.NodeID() {
		http.Error(w, "cannot pair a desk with itself", http.StatusBadRequest)
		return
	}
	var body struct {
		Code        string `json:"code"`
		DisplayName string `json:"display_name"`
		CircleName  string `json:"circle_name"`
		Addr        string `json:"addr"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "unreadable invitation", http.StatusBadRequest)
		return
	}
	if err := validateAddr(body.Addr, s.allowLoopback); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.repo.ConsumeInvite(r.Context(), strings.TrimSpace(body.Code), time.Now()); err != nil {
		http.Error(w, "invitation is expired or already used", http.StatusForbidden)
		return
	}
	if _, err := s.repo.UpsertPeer(r.Context(), Peer{NodeID: clientID, DisplayName: cleanName(body.DisplayName), CircleName: cleanName(body.CircleName), Addr: body.Addr}); err != nil {
		http.Error(w, "could not store peer", http.StatusInternalServerError)
		return
	}
	ident, err := s.Identity(r.Context())
	if err != nil {
		http.Error(w, "desk identity unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"node_id":      s.NodeID(),
		"display_name": ident.DisplayName,
		"circle_name":  ident.CircleName,
		"addr":         s.Addr(),
	})
}

func (s *Service) handleInbox(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	clientID, err := s.peerCert(r)
	if err != nil {
		http.Error(w, "missing desk certificate", http.StatusUnauthorized)
		return
	}
	peer, err := s.repo.PeerByNode(r.Context(), clientID)
	if err != nil {
		http.Error(w, "not invited", http.StatusForbidden)
		return
	}
	var body struct {
		Token    string `json:"token"`
		AppName  string `json:"app_name"`
		Headline string `json:"headline"`
		Author   string `json:"author"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.Token) == "" {
		http.Error(w, "unreadable share", http.StatusBadRequest)
		return
	}
	if err := s.repo.PutInbox(r.Context(), InboxItem{
		AuthorNode: clientID,
		AuthorName: cleanName(body.Author),
		Token:      strings.TrimSpace(body.Token),
		AppName:    cleanName(body.AppName),
		Headline:   cleanName(body.Headline),
	}); err != nil {
		http.Error(w, "could not store share", http.StatusInternalServerError)
		return
	}
	if addr := r.Header.Get("X-Oozie-Addr"); validateAddr(addr, s.allowLoopback) == nil && addr != peer.Addr {
		peer.Addr = addr
		_, _ = s.repo.UpsertPeer(r.Context(), peer)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Service) handleShare(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	clientID, err := s.peerCert(r)
	if err != nil {
		http.Error(w, "missing desk certificate", http.StatusUnauthorized)
		return
	}
	token := r.PathValue("token")
	_, owner, err := s.repo.GrantPeer(r.Context(), token)
	if err != nil || owner != clientID {
		http.Error(w, "share link is off", http.StatusNotFound)
		return
	}
	peer, err := s.repo.PeerByNode(r.Context(), clientID)
	if err != nil {
		http.Error(w, "share link is off", http.StatusNotFound)
		return
	}
	var appID int64
	grants, _ := s.repo.ListGrants(r.Context())
	for _, g := range grants {
		if g.Token == token {
			appID = g.AppID
			break
		}
	}
	if appID == 0 {
		http.Error(w, "share link is off", http.StatusNotFound)
		return
	}
	rec, err := s.projects.ExportRecipe(r.Context(), appID)
	if err != nil {
		http.Error(w, "recipe unavailable", http.StatusNotFound)
		return
	}
	rec.IconPNG = ""
	if addr := r.Header.Get("X-Oozie-Addr"); validateAddr(addr, s.allowLoopback) == nil {
		peer.Addr = addr
		_, _ = s.repo.UpsertPeer(r.Context(), peer)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

// ShortNode is the fingerprint shown so two people can compare desks.
func ShortNode(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func (s *Service) Desk(ctx context.Context, text, errMsg, flash string) (Desk, error) {
	apps, err := s.projects.ListStoreApps(ctx, "", "")
	if err != nil {
		return Desk{}, err
	}
	inbox, _ := s.Inbox(ctx)
	peers, _ := s.Peers(ctx)
	grants, _ := s.Grants(ctx)
	setup := ""
	if errMsg == "" {
		setup = s.projects.SetupHint()
	}
	model, projectModels, signed := s.projects.ModelChoices(ctx)
	models := make([]ModelChoice, 0, len(projectModels))
	for _, m := range projectModels {
		models = append(models, ModelChoice{Provider: m.Provider, ID: m.ID, Full: m.Full})
	}
	return Desk{
		Text: text, Err: errMsg, Flash: flash, Setup: setup,
		Apps: apps, Inbox: inbox, Peers: peers, Grants: grants,
		Insights: s.projects.Insights(ctx),
		Connect:  s.Addr() != "", Addr: s.Addr(),
		Model: model, Models: models, Signed: signed,
	}, nil
}
