package storecore

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// maxComponent bounds an encoded path component, below the 255-byte file
// name limit of common file systems with room for a ".json" suffix.
const maxComponent = 240

// TempPrefix starts every temporary file name. Encoded components never
// start with '.', so no record's name can collide with it, and every
// reader ignores names that carry it (ADR 0005, item 13).
const TempPrefix = ".tmp-"

// encode maps an identifier to a single safe path component. Lowercase
// letters, digits, '-' and '_' stand for themselves; every other byte,
// uppercase letters included, becomes %xx in lowercase hex. The result is
// therefore distinct for distinct inputs on case-insensitive file systems
// too, never contains a separator, and never starts with '.'.
func encode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02x", c)
	}
	return b.String()
}

// decode reverses encode. It reports false for a name encode cannot
// produce.
func decode(name string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c != '%' {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", false
			}
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(name) {
			return "", false
		}
		v, err := strconv.ParseUint(name[i+1:i+3], 16, 8)
		if err != nil || strings.ToLower(name[i+1:i+3]) != name[i+1:i+3] {
			return "", false
		}
		b.WriteByte(byte(v))
		i += 2
	}
	return b.String(), true
}

// component encodes an identifier, refusing an empty or oversized one.
func component(what, s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%w: empty %s", store.ErrInvalid, what)
	}
	e := encode(s)
	if len(e) > maxComponent {
		return "", fmt.Errorf("%w: %s too long (%d bytes encoded, limit %d)", store.ErrInvalid, what, len(e), maxComponent)
	}
	return e, nil
}

func seqName(seq uint64) string { return fmt.Sprintf("%020d.json", seq) }

func parseSeqName(name string) (uint64, bool) {
	base, ok := strings.CutSuffix(name, ".json")
	if !ok || len(base) != 20 {
		return 0, false
	}
	n, err := strconv.ParseUint(base, 10, 64)
	return n, err == nil
}

// Paths of the records (ADR 0005, item 13). Every argument is an encoded
// component.

func instanceDir(inst string) string        { return "instances/" + inst }
func ownershipPath(inst string) string      { return instanceDir(inst) + "/ownership.json" }
func checkpointDir(inst string) string      { return instanceDir(inst) + "/checkpoints" }
func runIndexDir(inst string) string        { return instanceDir(inst) + "/runs" }
func runIndexPath(inst, trig string) string { return runIndexDir(inst) + "/" + trig + ".json" }
func runPath(run string) string             { return "runs/" + run + "/run.json" }
func recordDir(run string) string           { return "runs/" + run + "/record" }
func objectDir(ns string) string            { return "objects/" + ns }
func objectPath(ns, key string) string      { return objectDir(ns) + "/" + key + ".json" }
func artifactPath(digest string) string     { return "artifacts/" + digest }
func artifactMetaPath(d string) string      { return "artifacts/" + d + ".json" }
func triggerDir(inst string) string         { return "triggers/" + inst }
