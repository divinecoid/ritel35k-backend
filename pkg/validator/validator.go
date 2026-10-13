// Package validator wraps go-playground/validator and converts failures to apperr.
package validator

import (
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/ritel35k/backend/pkg/apperr"
)

var v = func() *validator.Validate {
	x := validator.New(validator.WithRequiredStructEnabled())
	x.RegisterTagNameFunc(func(f reflect.StructField) string {
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "-" || name == "" {
			return f.Name
		}
		return name
	})
	return x
}()

// Struct validates s and returns an *apperr.Error listing field problems.
func Struct(s any) error {
	err := v.Struct(s)
	if err == nil {
		return nil
	}
	ve, ok := err.(validator.ValidationErrors)
	if !ok {
		return apperr.BadRequest(err.Error())
	}
	fields := make(map[string]string, len(ve))
	for _, fe := range ve {
		field := fe.Namespace()
		if i := strings.Index(field, "."); i >= 0 {
			field = field[i+1:]
		}
		msg := fe.Tag()
		if fe.Param() != "" {
			msg += "=" + fe.Param()
		}
		fields[field] = msg
	}
	return apperr.Validation(fields)
}
