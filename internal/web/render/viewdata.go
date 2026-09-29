package render

type ViewData struct {
	Title   string
	Content string // template name the layout renders as the page body
	Flash   string
	Err     string // error notice — rendered in error styling, not success
	Theme   string // saved appearance setting, rendered as data-theme
	Style   string // saved style profile, rendered as data-style
	Data    map[string]any
}
