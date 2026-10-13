package organization

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ritel35k/backend/pkg/apperr"
)

var (
	codeRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,31}$`)
	labelRe = regexp.MustCompile(`[^a-z0-9_]`)
)

func ValidCode(code string) bool { return codeRe.MatchString(code) }

// PathLabel turns an immutable unit code into an ltree label.
func PathLabel(code string) string { return labelRe.ReplaceAllString(strings.ToLower(code), "_") }

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func notFoundOr(err error, entity string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.NotFound(entity)
	}
	return err
}

var _ = context.Background
