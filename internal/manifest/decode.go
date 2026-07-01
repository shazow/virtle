package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

func Load(r io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return LoadBytes(data, "")
}

func LoadBytes(data []byte, name string) (*Manifest, error) {
	doc, err := DecodeDocumentBytes(data, name)
	if err != nil {
		return nil, err
	}
	return doc.Manifest()
}

func DecodeDocumentBytes(data []byte, name string) (Document, error) {
	var doc Document
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

func UpdateWorkingDirBytes(data []byte, name string, workingDir string) ([]byte, error) {
	var doc Document
	isTOML := manifestLooksTOML(data, name)
	var err error
	if isTOML {
		err = decodeTOML(data, &doc)
	} else {
		err = decodeJSON(data, &doc)
	}
	if err != nil {
		return nil, err
	}
	doc.WorkingDir = workingDir
	if isTOML {
		var out bytes.Buffer
		if err := toml.NewEncoder(&out).Encode(doc); err != nil {
			return nil, fmt.Errorf("encode manifest: %w", err)
		}
		return out.Bytes(), nil
	}
	updated, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return append(updated, '\n'), nil
}

func decodeJSON(data []byte, doc *Document) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(doc); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != nil {
		if err.Error() == "EOF" {
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
