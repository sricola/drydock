package gateway

import (
	"context"
	"net/http"
)

func contextWith(r *http.Request, l *Lease) context.Context {
	return context.WithValue(r.Context(), ctxKey{}, l)
}
