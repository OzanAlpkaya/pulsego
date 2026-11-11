package domain_test

import (
	"testing"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

func TestErrorKindValid(t *testing.T) {
	t.Parallel()

	kinds := domain.ErrorKinds()
	if got, want := len(kinds), 8; got != want {
		t.Fatalf("len(ErrorKinds()) = %d, want %d", got, want)
	}
	for _, kind := range kinds {
		if !kind.Valid() {
			t.Errorf("ErrorKind(%q).Valid() = false, want true", kind)
		}
	}

	for _, kind := range []domain.ErrorKind{"", "TIMEOUT", "http", "dns ", "unexpected-status"} {
		if kind.Valid() {
			t.Errorf("ErrorKind(%q).Valid() = true, want false", kind)
		}
	}
}
