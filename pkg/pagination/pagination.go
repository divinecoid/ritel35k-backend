// Package pagination parses page/per_page query params and builds response meta.
package pagination

import (
	"net/http"
	"strconv"
)

const (
	DefaultPerPage = 25
	MaxPerPage     = 200
)

type Params struct {
	Page    int
	PerPage int
}

func (p Params) Limit() int32  { return int32(p.PerPage) }
func (p Params) Offset() int32 { return int32((p.Page - 1) * p.PerPage) }

func FromRequest(r *http.Request) Params {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	per, _ := strconv.Atoi(q.Get("per_page"))
	if page < 1 {
		page = 1
	}
	if per < 1 {
		per = DefaultPerPage
	}
	if per > MaxPerPage {
		per = MaxPerPage
	}
	return Params{Page: page, PerPage: per}
}

type Meta struct {
	Page       int   `json:"page"`
	PerPage    int   `json:"per_page"`
	Total      int64 `json:"total"`
	TotalPages int   `json:"total_pages"`
}

func NewMeta(p Params, total int64) Meta {
	pages := int((total + int64(p.PerPage) - 1) / int64(p.PerPage))
	return Meta{Page: p.Page, PerPage: p.PerPage, Total: total, TotalPages: pages}
}
