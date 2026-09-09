// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIMutationRejectsSimpleContentType(t *testing.T) {
	s := new(tmemeServer)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/macro", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "text/plain")
	s.APIMux().ServeHTTP(w, r)
	if got, want := w.Code, http.StatusUnsupportedMediaType; got != want {
		t.Errorf("status code: got %d, want %d", got, want)
	}
}
