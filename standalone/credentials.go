package standalone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"sort"

	"github.com/SnapdragonPartners/maestro-agents/agent"
)

// credentials are the host's credentials, read once when run starts
// (MODE-5, ADR 0017 item 19).
type credentials struct {
	values    map[string]agent.Secret
	principal string
}

// loadCredentials reads the configured environment variables and secrets
// file. A name in both, an unset or empty variable, an empty value and a
// secrets file its group or others can access are errors.
func loadCredentials(c *CredentialsConfig, principal string, getenv func(string) string) (*credentials, error) {
	creds := &credentials{values: map[string]agent.Secret{}, principal: principal}
	var errs []error
	if c.File != "" {
		file, err := readSecretsFile(c.File)
		if err != nil {
			return nil, err
		}
		for _, name := range sortedKeys(file) {
			if _, dup := c.Env[name]; dup {
				errs = append(errs, fmt.Errorf("credential %q is in both credentials.env and the secrets file", name))
				continue
			}
			if file[name] == "" {
				errs = append(errs, fmt.Errorf("credential %q is empty in the secrets file", name))
				continue
			}
			creds.values[name] = agent.NewSecret(file[name])
		}
	}
	for _, name := range sortedKeys(c.Env) {
		v := getenv(c.Env[name])
		if v == "" {
			errs = append(errs, fmt.Errorf("credential %q: environment variable %s is unset or empty", name, c.Env[name]))
			continue
		}
		creds.values[name] = agent.NewSecret(v)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err //nolint:wrapcheck // the caller says these are credentials
	}
	return creds, nil
}

// readSecretsFile reads a secrets file: a JSON object from name to value,
// decoded strictly, that only its owner can access.
func readSecretsFile(path string) (map[string]string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the file
	if err != nil {
		return nil, fmt.Errorf("secrets file: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("secrets file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("secrets file %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("secrets file %s is accessible by its group or others (mode %04o); make it owner-only (chmod 600)", path, fi.Mode().Perm())
	}
	data, err := readLimited(f, maxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("secrets file %s: %w", path, err)
	}
	var m map[string]string
	if err := decodeStrict(data, &m); err != nil || m == nil {
		// The decoder's error is not passed on: a syntax error quotes the
		// character it stopped at, which can be part of a secret.
		return nil, fmt.Errorf("secrets file %s is not a JSON object from unique names to string values", path)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("secrets file %s holds no credentials", path)
	}
	return m, nil
}

// readLimited reads r to its end, refusing more than limit bytes.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller names the file
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return data, nil
}

// get returns a credential by name, without a principal check: the host's
// own use (the model route).
func (c *credentials) get(name string) (agent.Secret, bool) {
	s, ok := c.values[name]
	return s, ok
}

// provider is the credential provider tools reach through TOOL-8: only
// the configured names, only for runs under the configured principal.
func (c *credentials) provider(_ context.Context, rc *agent.RunContext, name string) (agent.Secret, error) {
	if rc == nil || rc.Principal != c.principal {
		return agent.Secret{}, fmt.Errorf("%w: %q (the run's principal is not this configuration's)", agent.ErrNoCredential, name)
	}
	s, ok := c.values[name]
	if !ok {
		return agent.Secret{}, fmt.Errorf("%w: %q", agent.ErrNoCredential, name)
	}
	return s, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return slices.Clip(keys)
}
