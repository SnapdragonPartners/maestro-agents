package storecore

import (
	"errors"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

func TestEncodeRoundTripAndSafety(t *testing.T) {
	inputs := []string{"a", "A", "inst-1", "a/b", "..", ".", ".tmp-x", "%41", "a b", "ü", "\x00", "LOCK", "x.json"}
	seen := map[string]string{}
	for _, in := range inputs {
		enc := encode(in)
		if strings.ContainsAny(enc, "/\\.") || strings.HasPrefix(enc, TempPrefix) {
			t.Errorf("encode(%q) = %q is not a safe component", in, enc)
		}
		if enc != strings.ToLower(enc) {
			t.Errorf("encode(%q) = %q is not case-insensitive safe", in, enc)
		}
		if prev, dup := seen[enc]; dup {
			t.Errorf("encode(%q) = encode(%q) = %q", in, prev, enc)
		}
		seen[enc] = in
		if dec, ok := decode(enc); !ok || dec != in {
			t.Errorf("decode(encode(%q)) = %q, %v", in, dec, ok)
		}
	}
	for _, bad := range []string{"A", "%4", "%4G", "%4A", ".x", "a.json"} {
		if _, ok := decode(bad); ok {
			t.Errorf("decode(%q) accepted a name encode cannot produce", bad)
		}
	}
}

func TestComponentLimits(t *testing.T) {
	if _, err := component("key", ""); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("empty component = %v", err)
	}
	if _, err := component("key", strings.Repeat("a", maxComponent)); err != nil {
		t.Errorf("component at the limit = %v", err)
	}
	if _, err := component("key", strings.Repeat("a", maxComponent+1)); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("oversized component = %v", err)
	}
}

func TestSeqNames(t *testing.T) {
	for _, n := range []uint64{1, 9, 10, 1<<64 - 1} {
		if got, ok := parseSeqName(seqName(n)); !ok || got != n {
			t.Errorf("parseSeqName(seqName(%d)) = %d, %v", n, got, ok)
		}
	}
	if seqName(9) >= seqName(10) {
		t.Error("sequence names do not sort numerically")
	}
	for _, bad := range []string{"1.json", "00000000000000000001", "0000000000000000000x.json"} {
		if _, ok := parseSeqName(bad); ok {
			t.Errorf("parseSeqName(%q) accepted", bad)
		}
	}
}
