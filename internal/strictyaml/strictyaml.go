// Package strictyaml decodes YAML configuration with unknown keys treated as
// fatal errors. PolarBEAM runs in air-gapped environments where a silently
// ignored (misspelled, misplaced) config key can go unnoticed for months;
// every key must therefore map to a known field or loading fails.
package strictyaml

import (
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadFile decodes the YAML file at path into out. Unknown keys, type
// mismatches, and multiple YAML documents are all errors.
func LoadFile(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	defer f.Close()
	if err := decode(f, out); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	return nil
}

func decode(r io.Reader, out any) error {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("file is empty")
		}
		return err
	}
	// A second document would be silently ignored by a single Decode call.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple YAML documents are not allowed")
	}
	return nil
}
