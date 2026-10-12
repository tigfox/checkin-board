package radiobackend_test

import (
	"checkin-board/internal/app"
	"checkin-board/internal/radiobackend"
	"checkin-board/internal/web"
)

// The backend stands in for graywolf wherever the engines and the web
// pages use it.
var (
	_ app.Graywolf = (*radiobackend.Backend)(nil)
	_ web.Graywolf = (*radiobackend.Backend)(nil)
)
