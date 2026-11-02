package localize

import (
	"encoding/json"
	"fmt"

	"sigs.k8s.io/yaml"
)

// unmarshalYAML converts YAML bytes to Go via JSON intermediate form.
func unmarshalYAML(data []byte, out any) error {
	jsonBytes, err := yaml.YAMLToJSON(data)
	if err != nil {
		return fmt.Errorf("yaml to json conversion failed: %w", err)
	}

	return json.Unmarshal(jsonBytes, out)
}
