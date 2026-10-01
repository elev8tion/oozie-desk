package projects

import (
	"context"
	"log"
	"strings"
	"time"
	"unicode"

	"oozie-desk/internal/agent/pi"
	"oozie-desk/internal/build"
)

// The wish inbox and its nightly build fairy: ideas dropped in during the
// day become working, published apps overnight. Wishes build in trusted
// projects — an unattended run can't answer permission prompts.

func (s *Service) AddWish(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return ErrValidation{"Describe the tool you wish existed."}
	}
	return s.repo.AddWish(ctx, text)
}

func (s *Service) DeleteWish(ctx context.Context, id int64) error {
	return s.repo.DeleteWish(ctx, id)
}

func (s *Service) ListWishes(ctx context.Context) ([]Wish, error) {
	return s.repo.ListWishes(ctx)
}

// BuildWish grants a wish now: project, agent build, and — when the agent
// finishes — an automatic publish. Returns the project id for the make-wait screen.
func (s *Service) BuildWish(ctx context.Context, id int64) (int64, error) {
	wish, err := s.repo.GetWish(ctx, id)
	if err != nil {
		return 0, err
	}
	if wish.Status == "building" {
		return 0, ErrValidation{"This wish is already being built."}
	}
	name := wishProjectName(wish.Text)
	project, err := s.CreateProject(ctx, name, "", true)
	if err != nil {
		return 0, err
	}
	// Auto-start so overnight prototypes are running by morning.
	draft := PublishDraft{ProjectID: project.ID, AppName: name, Headline: wishHeadline(wish.Text), Description: wish.Text, PublishTarget: "public", Visibility: "unlisted", ScreenshotManifest: "[]", AutoInstall: true}
	if err := s.repo.SaveDraft(ctx, draft); err != nil {
		return 0, err
	}
	msg := wishBuildMessage(wish.Text)
	requestID, err := s.sendAgentMessage(ctx, project.ID, "build", msg)
	if err != nil {
		_ = s.repo.SettleWish(ctx, id, "failed", err.Error())
		return 0, err
	}
	s.wishByRequest.Store(requestID, id)
	if err := s.repo.SetWishBuilding(ctx, id, project.ID); err != nil {
		return 0, err
	}
	return project.ID, nil
}

// settleWish closes the fairy loop: agent done → publish → wish granted.
func (s *Service) settleWish(projectID, requestID int64, status string) {
	v, ok := s.wishByRequest.Load(requestID)
	if !ok {
		return
	}
	wishID := v.(int64)
	ctx := context.Background()
	if status != "completed" {
		if s.retryWishAfterModel(ctx, projectID, requestID, wishID) {
			return
		}
		s.wishByRequest.Delete(requestID)
		s.wishRetryN.Delete(wishID)
		_ = s.repo.SettleWish(ctx, wishID, "failed", "the agent run ended with status "+status)
		return
	}
	if s.retryIncompleteScaffold(ctx, projectID, requestID, func(newID int64) {
		s.wishByRequest.Delete(requestID)
		s.wishByRequest.Store(newID, wishID)
	}) {
		return
	}
	s.wishByRequest.Delete(requestID)
	s.wishRetryN.Delete(wishID)
	s.incompleteScaffold.Delete(projectID)
	// If the agent finished without leaving anything buildable (it may
	// have declined the wish), fail with its own words instead of the
	// misleading "no go.mod" publish error.
	if project, perr := s.repo.GetProject(ctx, projectID); perr == nil {
		if wd, werr := resolveWorkdir(project); werr == nil && !build.Buildable(wd) {
			msg, _ := s.repo.FinalAssistantMessage(ctx, requestID)
			_ = s.repo.SettleWish(ctx, wishID, "failed", wishNoCodeError(msg))
			return
		}
	}
	err := s.publish(ctx, projectID, func(_ int64, buildErr error) {
		if buildErr != nil {
			_ = s.repo.SettleWish(ctx, wishID, "failed", "built by the agent but publish failed: "+buildErr.Error())
			return
		}
		_ = s.repo.SettleWish(ctx, wishID, "built", "")
		log.Printf("wish %d granted: tool published", wishID)
	})
	if err != nil {
		_ = s.repo.SettleWish(ctx, wishID, "failed", "publish could not start: "+err.Error())
	}
}

const maxWishModelRetries = 4

// retryWishAfterModel hops a wish build to the next model after overload/credit.
func (s *Service) retryWishAfterModel(ctx context.Context, projectID, requestID, wishID int64) bool {
	n := 0
	if v, ok := s.wishRetryN.Load(wishID); ok {
		n, _ = v.(int)
	}
	if n >= maxWishModelRetries {
		return false
	}
	_, errMsg, err := s.repo.RequestStatus(ctx, requestID)
	if err != nil || !pi.ModelRejected(errMsg) {
		return false
	}
	session, serr := s.repo.GetSession(ctx, projectID)
	if serr != nil {
		return false
	}
	next, nerr := s.modelForRetry(ctx, session.Model)
	if nerr != nil || !realHop(session.Model, next) {
		log.Printf("wish %d: refusal on %s — no other model, not hopping", wishID, session.Model)
		return false
	}
	if err := s.pinSessionModel(ctx, projectID, next); err != nil {
		return false
	}
	msg, err := s.repo.FirstUserMessage(ctx, requestID)
	if err != nil || strings.TrimSpace(msg) == "" {
		return false
	}
	s.wishByRequest.Delete(requestID)
	newID, err := s.sendAgentMessage(ctx, projectID, "build", msg)
	if err != nil || newID == 0 {
		s.wishRetryN.Store(wishID, n+1)
		return false
	}
	s.wishRetryN.Store(wishID, n+1)
	s.wishByRequest.Store(newID, wishID)
	log.Printf("wish %d: refusal on %s request %d — switched to %s as request %d (attempt %d)", wishID, session.Model, requestID, next, newID, n+1)
	return true
}

// fairyLoop wakes every minute; at the configured hour it takes up to
// three pending wishes and starts granting them.
func (s *Service) fairyLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	lastRun := ""
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			settings, err := s.repo.GetSettings(ctx)
			if err != nil || !settings.FairyEnabled || now.Local().Hour() != settings.FairyHour {
				continue
			}
			day := now.Local().Format("2006-01-02")
			if day == lastRun {
				continue
			}
			lastRun = day
			s.runNightShift(ctx)
		}
	}
}

func (s *Service) runNightShift(ctx context.Context) {
	wishes, err := s.repo.PendingWishes(ctx, 3)
	if err != nil {
		log.Printf("night shift: %v", err)
		return
	}
	if len(wishes) == 0 {
		return
	}
	log.Printf("night shift: granting %d wish(es)", len(wishes))
	for _, w := range wishes {
		if _, err := s.BuildWish(ctx, w.ID); err != nil {
			log.Printf("night shift: wish %d: %v", w.ID, err)
		}
	}
}

// wishProjectName distills a wish into a short project/app name.
func wishProjectName(text string) string {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	stop := map[string]bool{"a": true, "an": true, "the": true, "that": true, "for": true, "app": true, "me": true, "my": true, "i": true, "want": true, "wish": true, "wished": true, "build": true, "make": true, "to": true, "of": true, "with": true, "and": true, "had": true, "have": true, "has": true}
	var kept []string
	for _, w := range words {
		if !stop[strings.ToLower(w)] {
			kept = append(kept, strings.Title(strings.ToLower(w)))
		}
		if len(kept) == 3 {
			break
		}
	}
	if len(kept) == 0 {
		return "Wish " + time.Now().Format("Jan 2")
	}
	return strings.Join(kept, " ")
}

// wishNoCodeError explains a wish that "completed" with nothing to build,
// quoting the agent's final message so the real reason surfaces.
func wishNoCodeError(agentMsg string) string {
	msg := strings.Join(strings.Fields(agentMsg), " ")
	if msg == "" {
		return "the agent finished without producing a tool and left no message — see the project timeline"
	}
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "…"
	}
	return "the agent finished without producing a tool. It said: “" + msg + "” — see the project timeline for the full conversation"
}

// wishHeadline is the wish's first sentence, capped for the store card.
func wishHeadline(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexAny(text, ".!?\n"); i > 0 {
		text = text[:i]
	}
	if len(text) > 80 {
		text = text[:77] + "…"
	}
	if text == "" {
		return "An overnight prototype"
	}
	return text
}
