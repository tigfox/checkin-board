package store

import (
	"errors"
	"strings"

	"gorm.io/gorm"
)

var (
	// ErrInvalidInput: a store call got data that fails validation
	// (bad bib, checkpoint code, name, ...). Maps to HTTP 400.
	ErrInvalidInput = errors.New("store: invalid input")
	// ErrInvalidSettings: Settings.Validate failed. Maps to HTTP 400.
	ErrInvalidSettings = errors.New("store: invalid settings")
	// ErrNotFound: the addressed row doesn't exist. Maps to HTTP 404.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict: a unique key is already taken. Maps to HTTP 409.
	ErrConflict = errors.New("store: conflict")
	// ErrAlreadyVoided: the entry has already been voided. Maps to HTTP 409.
	ErrAlreadyVoided = errors.New("store: entry already voided")
)

// mapDBError translates driver errors into the package's sentinels so
// callers (and REST handlers) never see raw SQLite strings.
func mapDBError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return ErrNotFound
	case strings.Contains(err.Error(), "UNIQUE constraint failed"):
		return ErrConflict
	default:
		return err
	}
}

func rowsOrNotFound(res *gorm.DB) error {
	if res.Error != nil {
		return mapDBError(res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
