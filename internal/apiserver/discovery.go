package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"connectrpc.com/connect"

	discoveryv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/discovery/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/schema"
	"github.com/YukinaMochizuki/hangar/internal/buildinfo"
)

// docsBase is where the repo's docs are read at a revision, which the
// comments the schemas carry name by path.
const docsBase = "https://github.com/YukinaMochizuki/hangar/blob/"

var (
	// docPath is a path of the repo's docs, standing on its own: not part of
	// a longer path or a URL, so a link already made is left as it is.
	docPath = regexp.MustCompile(`(^|[^A-Za-z0-9_./:-])(docs/[A-Za-z0-9_./-]*?\.md)\b`)
	// revision is a commit's full SHA-1 or SHA-256 name.
	revision = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	// sentenceEnd is a full stop that starts a new sentence, which an
	// abbreviation such as e.g. does not.
	sentenceEnd = regexp.MustCompile(`\.\s+[A-Z]`)
)

// discovery serves DiscoveryService; see docs/decisions/api-schema.md.
type discovery struct {
	s *Server
	// schemas are the served JSON Schemas by apiVersion and kind, with their
	// docs as links.
	schemas map[[2]string]string
	// resources is what ListAPIResources answers, the same for everyone.
	resources []*discoveryv1alpha1.APIResource
}

// newDiscovery panics if a kind has no schema, or one that is not JSON,
// which only generated code out of step with itself causes.
func newDiscovery(s *Server) *discovery {
	ref := docsRef(buildinfo.Version())
	d := &discovery{s: s, schemas: map[[2]string]string{}}
	for _, r := range registry.All {
		raw, ok := schema.Get(r.Group, r.Version, r.Kind)
		if !ok {
			panic("no schema for " + r.Kind + "; run make gen")
		}
		js := linkDocs(string(raw), ref)
		var top struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal([]byte(js), &top); err != nil {
			panic(fmt.Sprintf("schema of %s: %v", r.Kind, err))
		}
		d.schemas[[2]string{r.APIVersion(), r.Kind}] = js
		d.resources = append(d.resources, &discoveryv1alpha1.APIResource{
			ApiVersion:  r.APIVersion(),
			Kind:        r.Kind,
			Plural:      r.Plural,
			ShortNames:  r.ShortNames,
			Scope:       r.Scope,
			Description: firstSentence(top.Description),
		})
	}
	return d
}

// docsRef is the revision docs are linked at for a build of version: its
// commit, or main for a build of no commit or of uncommitted changes.
func docsRef(version string) string {
	if revision.MatchString(version) {
		return version
	}
	return "main"
}

// linkDocs turns every docs/... path in js into a link to the file at ref.
func linkDocs(js, ref string) string {
	return docPath.ReplaceAllString(js, "${1}"+docsBase+ref+"/${2}")
}

// firstSentence is the first sentence of s's first paragraph.
func firstSentence(s string) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if loc := sentenceEnd.FindStringIndex(s); loc != nil {
		return s[:loc[0]+1]
	}
	return s
}

// ListAPIResources answers any authenticated caller.
func (d *discovery) ListAPIResources(ctx context.Context, _ *connect.Request[discoveryv1alpha1.ListAPIResourcesRequest]) (*connect.Response[discoveryv1alpha1.ListAPIResourcesResponse], error) {
	if _, err := d.s.authenticated(ctx); err != nil {
		return nil, err
	}
	return connect.NewResponse(&discoveryv1alpha1.ListAPIResourcesResponse{Resources: d.resources}), nil
}

// GetSchema answers any authenticated caller.
func (d *discovery) GetSchema(ctx context.Context, req *connect.Request[discoveryv1alpha1.GetSchemaRequest]) (*connect.Response[discoveryv1alpha1.GetSchemaResponse], error) {
	if _, err := d.s.authenticated(ctx); err != nil {
		return nil, err
	}
	js, ok := d.schemas[[2]string{req.Msg.GetApiVersion(), req.Msg.GetKind()}]
	if !ok {
		var kinds []string
		for _, r := range registry.All {
			kinds = append(kinds, r.APIVersion()+" "+r.Kind)
		}
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no kind %q in apiVersion %q; the kinds are %s",
			req.Msg.GetKind(), req.Msg.GetApiVersion(), strings.Join(kinds, ", ")))
	}
	return connect.NewResponse(&discoveryv1alpha1.GetSchemaResponse{JsonSchema: js}), nil
}
