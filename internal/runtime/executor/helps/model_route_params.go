package helps

import (
	"sort"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/sjson"
)

// ApplyModelRouteParams writes the payload paths configured on the selected
// model-route target. Callers must invoke it before applying user payload rules.
func ApplyModelRouteParams(payload []byte, opts cliproxyexecutor.Options) []byte {
	if len(payload) == 0 || len(opts.Metadata) == 0 {
		return payload
	}
	params, ok := opts.Metadata[cliproxyexecutor.ModelRouteParamsMetadataKey].(map[string]any)
	if !ok || len(params) == 0 {
		return payload
	}
	paths := make([]string, 0, len(params))
	for path := range params {
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		updated, errSet := sjson.SetBytes(payload, strings.TrimSpace(path), params[path])
		if errSet != nil {
			continue
		}
		payload = updated
	}
	return payload
}
