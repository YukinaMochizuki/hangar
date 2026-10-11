package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
)

// manifest is one object read from a file.
type manifest struct {
	source string
	r      registry.Resource
	obj    proto.Message
}

// readManifests reads every object in the given files, directories (their
// .yaml, .yml and .json files, in name order) or "-" for stdin.
func readManifests(paths []string, stdin io.Reader) ([]manifest, error) {
	var out []manifest
	for _, p := range paths {
		if p == "-" {
			ms, err := decodeManifests("stdin", stdin)
			if err != nil {
				return nil, err
			}
			out = append(out, ms...)
			continue
		}
		files, err := manifestFiles(p)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			ms, err := decodeManifests(f, bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			out = append(out, ms...)
		}
	}
	return out, nil
}

func manifestFiles(p string) ([]string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{p}, nil
	}
	var files []string
	err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml", ".json":
			if !d.IsDir() {
				files = append(files, path)
			}
		}
		return nil
	})
	slices.Sort(files)
	return files, err
}

// decodeManifests reads a stream of YAML (or JSON) documents. Each becomes the
// message its apiVersion and kind name, decoded strictly: an unknown field is
// an error, not something silently dropped.
func decodeManifests(source string, r io.Reader) ([]manifest, error) {
	dec := yaml.NewDecoder(r)
	var out []manifest
	for i := 1; ; i++ {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		where := fmt.Sprintf("%s, document %d", source, i)
		if err != nil {
			return nil, usageErr("%s: %v", where, err)
		}
		if doc == nil {
			continue
		}
		fields, ok := doc.(map[string]any)
		if !ok {
			return nil, usageErr("%s: not an object", where)
		}
		apiVersion, _ := fields["apiVersion"].(string)
		kind, _ := fields["kind"].(string)
		res, ok := registry.Lookup(apiVersion, kind)
		if !ok {
			return nil, usageErr("%s: unknown apiVersion %q and kind %q", where, apiVersion, kind)
		}
		js, err := json.Marshal(doc)
		if err != nil {
			return nil, usageErr("%s: %v", where, err)
		}
		obj := res.Message.New().Interface()
		if err := protojson.Unmarshal(js, obj); err != nil {
			return nil, usageErr("%s: %v", where, err)
		}
		out = append(out, manifest{source: where, r: res, obj: obj})
	}
}
