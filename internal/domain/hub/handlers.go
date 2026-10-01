package hub

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"oozie-desk/internal/web/render"
)

type Handlers struct {
	service  *Service
	renderer *render.Renderer
}

func NewHandlers(service *Service, renderer *render.Renderer) *Handlers {
	return &Handlers{service: service, renderer: renderer}
}

func (h *Handlers) page(w http.ResponseWriter, r *http.Request, title, content string, data map[string]any) {
	theme, style := "system", "graphite"
	if s, err := h.service.projects.GetSettings(r.Context()); err == nil {
		theme, style = s.Appearance, s.StyleProfile
	}
	h.renderer.HTML(w, 200, "layouts/base", render.ViewData{Title: title, Content: content, Theme: theme, Style: style, Data: data})
}

func (h *Handlers) Desk(w http.ResponseWriter, r *http.Request) {
	desk, err := h.service.Desk(r.Context(), r.URL.Query().Get("text"), r.URL.Query().Get("err"), r.URL.Query().Get("flash"))
	if err != nil {
		http.Error(w, "desk unavailable", http.StatusInternalServerError)
		return
	}
	h.page(w, r, "Oozie Desk", "pages/desk/index-content", map[string]any{"Desk": desk})
}

func (h *Handlers) People(w http.ResponseWriter, r *http.Request) {
	h.renderPeople(w, r, "", r.URL.Query().Get("err"), r.URL.Query().Get("flash"))
}

func (h *Handlers) renderPeople(w http.ResponseWriter, r *http.Request, invite, errMsg, flash string) {
	ident, err := h.service.Identity(r.Context())
	if err != nil {
		http.Error(w, "desk unavailable", http.StatusInternalServerError)
		return
	}
	ident.Addr = h.service.Addr()
	ident.Connect = ident.Addr != ""
	peers, _ := h.service.Peers(r.Context())
	h.page(w, r, "People · Oozie Desk", "pages/people/index-content", map[string]any{
		"Identity": ident,
		"Peers":    peers,
		"Invite":   invite,
		"Err":      errMsg,
		"Flash":    flash,
		"Short":    ShortNode(ident.NodeID),
	})
}

func (h *Handlers) CreateInvite(w http.ResponseWriter, r *http.Request) {
	invite, err := h.service.CreateInvite(r.Context())
	if err != nil {
		h.renderPeople(w, r, "", err.Error(), "")
		return
	}
	h.renderPeople(w, r, invite, "", "Invitation is live for 15 minutes. It works once.")
}

func (h *Handlers) AcceptInvite(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderPeople(w, r, "", "Could not read that form.", "")
		return
	}
	if err := h.service.AcceptInvite(r.Context(), r.FormValue("invite")); err != nil {
		h.renderPeople(w, r, "", err.Error(), "")
		return
	}
	http.Redirect(w, r, "/people?flash="+url.QueryEscape("Connected. You can share tools with this desk."), http.StatusSeeOther)
}

func (h *Handlers) Connect(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.FormValue("enabled") == "1" {
		if _, err := h.service.Connect(r.Context()); err != nil {
			h.back(w, r, "/people", err.Error())
			return
		}
		h.back(w, r, "/people", "")
		return
	}
	h.service.Disconnect()
	h.back(w, r, "/people", "")
}

func (h *Handlers) Revoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad peer", http.StatusBadRequest)
		return
	}
	if err := h.service.Revoke(r.Context(), id); err != nil {
		h.back(w, r, "/people", err.Error())
		return
	}
	http.Redirect(w, r, "/people?flash="+url.QueryEscape("Disconnected. Their share links no longer work."), http.StatusSeeOther)
}

func (h *Handlers) SidebarFragment(w http.ResponseWriter, r *http.Request) {
	ident, err := h.service.Identity(r.Context())
	if err != nil {
		http.Error(w, "desk unavailable", http.StatusInternalServerError)
		return
	}
	bar := Sidebar{Name: ident.DisplayName, Circle: ident.CircleName, Addr: h.service.Addr()}
	bar.Connect = bar.Addr != ""
	if peers, err := h.service.Peers(r.Context()); err == nil {
		bar.People = len(peers)
	}
	if apps, err := h.service.projects.InstalledApps(r.Context()); err == nil {
		bar.Running = len(apps)
		for _, app := range apps {
			if len(bar.Tools) == 3 {
				break
			}
			if app.PublicURL == "" {
				continue
			}
			bar.Tools = append(bar.Tools, SidebarTool{Name: app.Name, URL: "/run/" + strconv.FormatInt(app.ID, 10)})
		}
	}
	h.renderer.HTML(w, 200, "partials/desk/sidebar", render.ViewData{Data: map[string]any{"Sidebar": bar}})
}

func (h *Handlers) IdentityFragment(w http.ResponseWriter, r *http.Request) {
	ident, err := h.service.Identity(r.Context())
	if err != nil {
		http.Error(w, "desk unavailable", http.StatusInternalServerError)
		return
	}
	ident.Addr = h.service.Addr()
	ident.Connect = ident.Addr != ""
	h.renderer.HTML(w, 200, "partials/people/identity", render.ViewData{Data: map[string]any{"Identity": ident, "Short": ShortNode(ident.NodeID)}})
}

func (h *Handlers) SaveIdentity(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	first, initial := r.FormValue("first_name"), r.FormValue("last_initial")
	if err := h.service.SaveIdentity(r.Context(), first, initial, r.FormValue("circle_name")); err != nil {
		if r.Header.Get("HX-Request") == "true" {
			ident, _ := h.service.Identity(r.Context())
			ident.FirstName, ident.LastInitial = strings.TrimSpace(first), strings.TrimSpace(initial)
			h.renderer.HTML(w, 200, "partials/people/identity", render.ViewData{Err: err.Error(), Data: map[string]any{"Identity": ident, "Short": ShortNode(ident.NodeID)}})
			return
		}
		h.back(w, r, "/settings", err.Error())
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Trigger", "sidebarRefresh")
		h.IdentityFragment(w, r)
		return
	}
	http.Redirect(w, r, "/people?flash="+url.QueryEscape("Name saved."), http.StatusSeeOther)
}

func (h *Handlers) ShareFragment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad tool", http.StatusBadRequest)
		return
	}
	h.renderShare(w, r, id, "")
}

func (h *Handlers) renderShare(w http.ResponseWriter, r *http.Request, appID int64, errMsg string) {
	peers, _ := h.service.Peers(r.Context())
	grants, _ := h.service.Grants(r.Context())
	var mine []Grant
	for _, g := range grants {
		if g.AppID == appID {
			mine = append(mine, g)
		}
	}
	h.renderer.HTML(w, 200, "partials/desk/share", render.ViewData{Err: errMsg, Data: map[string]any{
		"AppID":   appID,
		"Peers":   peers,
		"Grants":  mine,
		"Connect": h.service.Addr() != "",
	}})
}

func (h *Handlers) ActivateShare(w http.ResponseWriter, r *http.Request) {
	appID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad tool", http.StatusBadRequest)
		return
	}
	_ = r.ParseForm()
	peerID, _ := strconv.ParseInt(r.FormValue("peer_id"), 10, 64)
	if err := h.service.ActivateShare(r.Context(), appID, peerID); err != nil {
		if r.Header.Get("HX-Request") == "true" {
			h.renderShare(w, r, appID, err.Error())
			return
		}
		h.back(w, r, "/", err.Error())
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		h.renderShare(w, r, appID, "")
		return
	}
	h.back(w, r, "/", "")
}

func (h *Handlers) StopShare(w http.ResponseWriter, r *http.Request) {
	appID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad tool", http.StatusBadRequest)
		return
	}
	_ = r.ParseForm()
	peerID, _ := strconv.ParseInt(r.FormValue("peer_id"), 10, 64)
	if err := h.service.StopShare(r.Context(), appID, peerID); err != nil {
		h.back(w, r, "/", err.Error())
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		h.renderShare(w, r, appID, "")
		return
	}
	h.back(w, r, "/", "")
}

func (h *Handlers) AcceptInbox(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad share", http.StatusBadRequest)
		return
	}
	project, err := h.service.AcceptInbox(r.Context(), id)
	if err != nil {
		h.back(w, r, "/", err.Error())
		return
	}
	http.Redirect(w, r, "/make/"+strconv.FormatInt(project.ID, 10), http.StatusSeeOther)
}

func (h *Handlers) DismissInbox(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad share", http.StatusBadRequest)
		return
	}
	_ = h.service.DismissInbox(r.Context(), id)
	h.back(w, r, "/", "")
}

func (h *Handlers) AcceptLink(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	project, err := h.service.AcceptLink(r.Context(), r.FormValue("link"))
	if err != nil {
		h.back(w, r, "/", err.Error())
		return
	}
	http.Redirect(w, r, "/make/"+strconv.FormatInt(project.ID, 10), http.StatusSeeOther)
}

func (h *Handlers) back(w http.ResponseWriter, r *http.Request, fallback, errMsg string) {
	target := fallback
	if ref := r.Referer(); ref != "" {
		if u, err := url.Parse(ref); err == nil && (u.Host == "" || u.Host == r.Host) && strings.HasPrefix(u.Path, "/") {
			target = u.Path
		}
	}
	if errMsg != "" {
		target += "?err=" + url.QueryEscape(errMsg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
