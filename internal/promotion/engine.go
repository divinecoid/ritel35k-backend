package promotion

import "github.com/ritel35k/backend/pkg/promoengine"

// The engine is a pure function that also runs on the store's edge agent (offline POS), so it lives in pkg/.
// These aliases keep the rest of the backend speaking in terms of the promotion package.
type (
	Type       = promoengine.Type
	Config     = promoengine.Config
	Scopes     = promoengine.Scopes
	Promotion  = promoengine.Promotion
	Line       = promoengine.Line
	Context    = promoengine.Context
	Applied    = promoengine.Applied
	LineResult = promoengine.LineResult
	Result     = promoengine.Result
)

const (
	Percent   = promoengine.Percent
	Fixed     = promoengine.Fixed
	BxGy      = promoengine.BxGy
	Bundle    = promoengine.Bundle
	Threshold = promoengine.Threshold
)

var Evaluate = promoengine.Evaluate
