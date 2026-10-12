package radiobackend

import "checkin-board/internal/app"

// The backend stands in for graywolf wherever the engines use it.
var _ app.Graywolf = (*Backend)(nil)
