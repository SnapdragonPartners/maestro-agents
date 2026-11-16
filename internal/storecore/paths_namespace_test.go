package storecore

import (
	"errors"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

func TestCheckNamespace(t *testing.T) {
	if err := CheckNamespace("intake"); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"", "\xff", strings.Repeat("N", 81), strings.Repeat("n", maxComponent+1)} {
		if err := CheckNamespace(ns); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%q: %v", ns, err)
		}
	}
	if err := CheckNamespace(strings.Repeat("n", maxComponent)); err != nil {
		t.Errorf("at the limit: %v", err)
	}
}
