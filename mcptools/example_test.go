package mcptools_test

import (
	"context"
	"net/http"

	"github.com/SnapdragonPartners/maestro-llms/mcp/mcpclient"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/mcptools"
)

// A host builds one Source per run's principal, with an http.Client whose
// transport adds and renews that principal's token, passes it to each
// activation of the run, and closes it when the run is done.
func ExampleNew() {
	var tokenClient *http.Client // the host's: its transport sets Authorization
	src := mcptools.New(func(context.Context) (mcpclient.Transport, error) {
		return mcpclient.StreamableHTTP("https://morris.example/mcp", tokenClient), nil
	}, mcptools.Options{})
	defer func() { _ = src.Close() }()

	in := agent.ActivationInput{
		ToolSources: map[string]agent.ToolSource{"morris": src},
		// ... the rest of the activation's input
	}
	_ = in
}
