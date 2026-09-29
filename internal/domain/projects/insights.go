package projects

import (
	"context"
	"fmt"
	"time"
)

// An Insight is oozie noticing something about your app ecosystem and
// proposing what to do about it — derived only from data oozie already
// owns (beacon events, wishes, store state). No screen watching.
type Insight struct {
	Text      string
	ActionURL string
	ActionTxt string
}

func (s *Service) Insights(ctx context.Context) []Insight {
	var out []Insight
	apps, err := s.repo.ListStoreApps(ctx, "", "")
	if err == nil {
		var top *StoreApp
		for i := range apps {
			app := &apps[i]
			if app.Dormant() {
				since := "a while"
				if app.LastLaunchAt != nil {
					since = fmt.Sprintf("%d days", int(time.Since(*app.LastLaunchAt).Hours()/24))
				} else if app.LastPublishedAt != nil {
					since = fmt.Sprintf("%d days", int(time.Since(*app.LastPublishedAt).Hours()/24))
				}
				out = append(out, Insight{
					Text:      fmt.Sprintf("%s is installed but hasn't been opened in %s. Retire it, or tell the agent what would make it worth opening.", app.Name, since),
					ActionURL: "/improve/" + app.BundleSlug, ActionTxt: "Make it better",
				})
			}
			if app.Installed && app.LaunchCount >= 5 && (top == nil || app.LaunchCount > top.LaunchCount) {
				top = app
			}
		}
		if top != nil {
			out = append(out, Insight{
				Text:      fmt.Sprintf("%s is your most-used app (%d launches). Heavy use earns a polish pass.", top.Name, top.LaunchCount),
				ActionURL: "/improve/" + top.BundleSlug, ActionTxt: "Polish it",
			})
		}
	}
	if wishes, err := s.repo.PendingWishes(ctx, 10); err == nil && len(wishes) > 0 {
		oldest := wishes[0]
		if time.Since(oldest.CreatedAt) > 3*24*time.Hour {
			out = append(out, Insight{
				Text:      fmt.Sprintf("%d wish(es) have been waiting since %s. Build one now, or let the night fairy handle them.", len(wishes), oldest.CreatedAt.Local().Format("Jan 2")),
				ActionURL: "/wishes", ActionTxt: "Open wishes",
			})
		}
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}
