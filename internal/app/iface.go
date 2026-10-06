package app

import "checkin-board/internal/graywolf"

var _ Graywolf = (*graywolf.Client)(nil)
