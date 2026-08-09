package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// NewDocument returns an empty Document with its scalar defaults seeded, the
// same starting point DecodeDocumentBytes uses. Documents built in code rather
// than decoded need it: DocumentWithDefaults cannot tell an omitted duration
// from an explicit zero, so the durations whose zero value is meaningful have
// to arrive already set.
func NewDocument() Document {
	var doc Document
	applyDefaultTags(&doc)
	return doc
}

func DecodeDocumentBytes(data []byte, name string) (Document, error) {
	doc := NewDocument()
	var err error
	if manifestLooksTOML(data, name) {
		err = decodeTOML(data, &doc)
	} else {
		err = decodeJSON(data, &doc)
	}
	if err != nil {
		return Document{}, err
	}
	return doc, nil
}

func decodeJSON(data []byte, doc *Document) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(doc); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("decode manifest: %w", err)
	}
	return fmt.Errorf("decode manifest: unexpected trailing data")
}

func decodeTOML(data []byte, doc *Document) error {
	metadata, err := toml.NewDecoder(bytes.NewReader(data)).Decode(doc)
	if err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		for _, key := range undecoded {
			if taggedTOMLKey(key) {
				continue
			}
			return fmt.Errorf("decode manifest: unknown key %s", key.String())
		}
	}
	return nil
}

func taggedTOMLKey(key toml.Key) bool {
	if len(key) == 0 {
		return false
	}
	if key[0] == "mounts" {
		return true
	}
	return len(key) > 1 && key[0] == "hotplug" && key[1] == "mounts"
}

func manifestLooksTOML(data []byte, name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".toml":
		return true
	case ".json":
		return false
	}
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && trimmed[0] != '{'
}
