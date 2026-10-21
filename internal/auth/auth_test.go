package auth

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/config"
)

func TestHashKey(t *testing.T) {
	// echo -n sk-dev-acme-support | sha256sum
	const want = "47c0c7636be2dab927068d6818497303daa92c2ffe25f39c8cd88b9b4326ef5b"
	if got := HashKey("sk-dev-acme-support"); got != want {
		t.Fatalf("HashKey = %s, want %s", got, want)
	}
}

func TestAuthenticate(t *testing.T) {
	a := New([]config.Tenant{
		{ID: "acme", APIKeys: []string{HashKey("key-a")}},
		{ID: "globex", APIKeys: []string{HashKey("key-b1"), HashKey("key-b2")}},
	})
	cases := []struct {
		header string
		tenant string
		err    error
	}{
		{"Bearer key-a", "acme", nil},
		{"bearer key-b2", "globex", nil},
		{"Bearer  key-b1 ", "globex", nil},
		{"", "", ErrMissingKey},
		{"Basic key-a", "", ErrMissingKey},
		{"Bearer ", "", ErrMissingKey},
		{"Bearer key-c", "", ErrInvalidKey},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		got, err := a.Authenticate(r)
		if !errors.Is(err, tc.err) {
			t.Errorf("%q: err = %v, want %v", tc.header, err, tc.err)
			continue
		}
		if err == nil && got.ID != tc.tenant {
			t.Errorf("%q: tenant = %s, want %s", tc.header, got.ID, tc.tenant)
		}
	}
}
