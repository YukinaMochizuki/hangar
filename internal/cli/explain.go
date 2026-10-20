package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	discoveryv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/discovery/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/discovery/v1alpha1/discoveryv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
)

// discoveryClient returns a client of the context's DiscoveryService. The
// kinds and schemas come from the server, so a CLI older than a kind still
// lists and explains it.
func (a *app) discoveryClient() (discoveryv1alpha1connect.DiscoveryServiceClient, error) {
	c, err := a.current()
	if err != nil {
		return nil, err
	}
	creds, err := a.credentials(c)
	if err != nil {
		return nil, err
	}
	hc, base := a.httpClient(c)
	return discoveryv1alpha1connect.NewDiscoveryServiceClient(hc, base, connect.WithInterceptors(creds)), nil
}

func (a *app) apiResourcesCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "api-resources",
		Short: "List the kinds the server serves",
		Args:  args(0, 0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if output != "" && output != "json" && output != "name" {
				return usageErr("api-resources prints json or name, not %q", output)
			}
			dc, err := a.discoveryClient()
			if err != nil {
				return err
			}
			res, err := dc.ListAPIResources(cmd.Context(), connect.NewRequest(&discoveryv1alpha1.ListAPIResourcesRequest{}))
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			switch output {
			case "json":
				items := []json.RawMessage{}
				for _, r := range res.Msg.GetResources() {
					js, err := marshalJSON(r)
					if err != nil {
						return err
					}
					items = append(items, js)
				}
				js, err := json.MarshalIndent(map[string]any{"items": items}, "", "  ")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(w, string(js))
				return err
			case "name":
				for _, r := range res.Msg.GetResources() {
					_, _ = fmt.Fprintln(w, r.GetPlural())
				}
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 4, 3, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tSHORTNAMES\tAPIVERSION\tSCOPE\tKIND")
			for _, r := range res.Msg.GetResources() {
				scope := "Global"
				if r.GetScope() == metav1.Scope_SCOPE_PROJECT {
					scope = "Project"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.GetPlural(), strings.Join(r.GetShortNames(), ","), r.GetApiVersion(), scope, r.GetKind())
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "json or name")
	return cmd
}

func (a *app) explainCmd() *cobra.Command {
	var output string
	var recursive bool
	cmd := &cobra.Command{
		Use:   "explain KIND[.FIELD...]",
		Short: "Describe a kind or one of its fields, from the server's schema",
		Long: `Describe a kind or one of its fields, from the server's schema.

FIELD names a field by its JSON name, as manifests write it, such as
container.spec.resources. A list or a map is explained by its items: in
container.spec.ports.port, ports is a list of ports.

With --recursive, every field below is listed by name and type. With
-o json, the JSON Schema of the kind or field is printed, with the
definitions it refers to under $defs.`,
		Args: args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "" && output != "json" {
				return usageErr("explain prints json, not %q", output)
			}
			kindName, path, _ := strings.Cut(args[0], ".")
			dc, err := a.discoveryClient()
			if err != nil {
				return err
			}
			res, err := dc.ListAPIResources(cmd.Context(), connect.NewRequest(&discoveryv1alpha1.ListAPIResourcesRequest{}))
			if err != nil {
				return err
			}
			r, err := serverKind(res.Msg.GetResources(), kindName)
			if err != nil {
				return err
			}
			sres, err := dc.GetSchema(cmd.Context(), connect.NewRequest(&discoveryv1alpha1.GetSchemaRequest{ApiVersion: r.GetApiVersion(), Kind: r.GetKind()}))
			if err != nil {
				return err
			}
			var root jsonSchema
			if err := json.Unmarshal([]byte(sres.Msg.GetJsonSchema()), &root); err != nil {
				return fmt.Errorf("the server's schema of %s: %w", r.GetKind(), err)
			}
			var fields []string
			if path != "" {
				fields = strings.Split(path, ".")
			}
			field, fields, err := root.at(fields)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if output == "json" {
				out := field
				if len(fields) > 0 {
					out = jsonSchema{}
					for k, v := range field {
						out[k] = v
					}
					out["$schema"] = root["$schema"]
					if defs := root.defsOf(field); len(defs) > 0 {
						out["$defs"] = defs
					}
				}
				enc := json.NewEncoder(w)
				enc.SetEscapeHTML(false)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			root.explain(w, r, fields, field, recursive)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "json for the JSON Schema")
	cmd.Flags().BoolVar(&recursive, "recursive", false, "list every field below, by name and type")
	return cmd
}

// serverKind finds the kind a command line names among the server's: by its
// kind in any case, its plural or a short name.
func serverKind(resources []*discoveryv1alpha1.APIResource, name string) (*discoveryv1alpha1.APIResource, error) {
	n := strings.ToLower(name)
	var names []string
	of := map[string]*discoveryv1alpha1.APIResource{}
	for _, r := range resources {
		for _, alias := range append([]string{strings.ToLower(r.GetKind()), r.GetPlural()}, r.GetShortNames()...) {
			if alias == n {
				return r, nil
			}
			names = append(names, alias)
			of[alias] = r
		}
	}
	msg := fmt.Sprintf("unknown kind %q", name)
	if c, ok := closest(name, names); ok {
		msg += fmt.Sprintf("; did you mean %s?", of[c].GetPlural())
	}
	return nil, usageErr("%s\nrun hangar api-resources for the kinds", msg)
}

// jsonSchema is one schema of a document the server serves, read as generic
// JSON so a kind or a keyword this CLI does not know still reads.
type jsonSchema map[string]any

func (s jsonSchema) str(key string) string {
	v, _ := s[key].(string)
	return v
}

func (s jsonSchema) sub(key string) (jsonSchema, bool) {
	v, ok := s[key].(map[string]any)
	return v, ok
}

// resolve follows s's $ref, if it has one, within root.
func (root jsonSchema) resolve(s jsonSchema) jsonSchema {
	for range 32 {
		ref := s.str("$ref")
		name, ok := strings.CutPrefix(ref, "#/$defs/")
		if !ok {
			return s
		}
		defs, _ := root.sub("$defs")
		d, ok := defs[name].(map[string]any)
		if !ok {
			return s
		}
		s = d
	}
	return s
}

// element is what a field holds one of: an item of a list, a value of a map,
// or the field itself.
func (root jsonSchema) element(s jsonSchema) jsonSchema {
	s = root.resolve(s)
	for range 32 {
		if items, ok := s.sub("items"); ok {
			s = root.resolve(items)
			continue
		}
		if v, ok := s.sub("additionalProperties"); ok {
			s = root.resolve(v)
			continue
		}
		return s
	}
	return s
}

// defsOf is every definition s refers to, and those they refer to in turn.
func (root jsonSchema) defsOf(s jsonSchema) map[string]any {
	all, _ := root.sub("$defs")
	out := map[string]any{}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if name, ok := strings.CutPrefix(jsonSchema(v).str("$ref"), "#/$defs/"); ok {
				if _, done := out[name]; !done && all[name] != nil {
					out[name] = all[name]
					walk(all[name])
				}
			}
			for _, sub := range v {
				walk(sub)
			}
		case []any:
			for _, sub := range v {
				walk(sub)
			}
		}
	}
	walk(map[string]any(s))
	return out
}

// at returns the schema of the field at path below root, as the property
// declares it, $ref and all, and the path by the names the schema gives the
// fields.
func (root jsonSchema) at(path []string) (jsonSchema, []string, error) {
	cur := root
	var names []string
	for _, name := range path {
		parent := root.element(cur)
		props, _ := parent.sub("properties")
		found := name
		next, ok := props[name].(map[string]any)
		if !ok {
			for k, v := range props {
				if strings.EqualFold(k, name) || strings.EqualFold(k, strings.ReplaceAll(name, "_", "")) {
					next, ok, found = v.(map[string]any), true, k
					break
				}
			}
		}
		if !ok {
			at := strings.Join(names, ".")
			if at == "" {
				at = root.str("title")
			}
			fieldList := sortedKeys(props)
			if len(fieldList) == 0 {
				return nil, nil, usageErr("%s has no fields", at)
			}
			msg := fmt.Sprintf("%s has no field %q", at, name)
			if c, ok := closest(name, fieldList); ok {
				msg += fmt.Sprintf("; did you mean %s?", c)
			}
			return nil, nil, usageErr("%s; it has %s", msg, strings.Join(fieldList, ", "))
		}
		names = append(names, found)
		cur = next
	}
	return cur, names, nil
}

// typeName is how a field's type reads, as kubectl explain writes it.
func (root jsonSchema) typeName(s jsonSchema) string {
	if ref := s.str("$ref"); ref != "" {
		return ref[strings.LastIndex(ref, ".")+1:]
	}
	if items, ok := s.sub("items"); ok {
		return "[]" + root.typeName(items)
	}
	if v, ok := s.sub("additionalProperties"); ok {
		return "map[string]" + root.typeName(v)
	}
	if _, ok := s["enum"]; ok {
		return "enum"
	}
	if s.str("contentEncoding") == "base64" {
		return "base64"
	}
	switch s.str("format") {
	case "date-time":
		return "timestamp"
	case "int32", "int64", "uint32", "uint64":
		return "integer"
	}
	if s.str("pattern") != "" && s.str("type") == "string" && strings.HasSuffix(s.str("pattern"), "s$") {
		return "duration"
	}
	if t := s.str("type"); t != "" {
		return t
	}
	return "object"
}

func (root jsonSchema) explain(w io.Writer, r *discoveryv1alpha1.APIResource, path []string, field jsonSchema, recursive bool) {
	_, _ = fmt.Fprintf(w, "KIND:     %s\nVERSION:  %s\n\n", r.GetKind(), r.GetApiVersion())
	if len(path) > 0 {
		_, _ = fmt.Fprintf(w, "FIELD:    %s <%s>\n\n", strings.Join(path, "."), root.typeName(field))
	}
	el := root.element(field)
	desc := field.str("description")
	if d := el.str("description"); d != "" && d != desc {
		desc = strings.TrimSpace(desc + "\n\n" + d)
	}
	if desc != "" {
		_, _ = fmt.Fprintln(w, "DESCRIPTION:")
		writeWrapped(w, desc, "    ", 80)
		_, _ = fmt.Fprintln(w)
	}
	if values, ok := el["enum"].([]any); ok {
		_, _ = fmt.Fprintln(w, "VALUES:")
		for _, v := range values {
			_, _ = fmt.Fprintf(w, "    %v\n", v)
		}
		_, _ = fmt.Fprintln(w)
	}
	props, _ := el.sub("properties")
	if len(props) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "FIELDS:")
	if recursive {
		root.tree(w, el, "  ", map[string]bool{})
		return
	}
	required := map[string]bool{}
	if req, ok := el["required"].([]any); ok {
		for _, n := range req {
			required[fmt.Sprint(n)] = true
		}
	}
	for _, name := range sortedKeys(props) {
		p, _ := props[name].(map[string]any)
		ps := jsonSchema(p)
		line := fmt.Sprintf("  %s\t<%s>", name, root.typeName(ps))
		if required[name] {
			line += " -required-"
		}
		if ro, _ := ps["readOnly"].(bool); ro {
			line += " -read-only-"
		}
		if c := ps.str("const"); c != "" {
			line += " = " + c
		}
		_, _ = fmt.Fprintln(w, line)
		d := ps.str("description")
		if d == "" {
			d = root.element(ps).str("description")
		}
		if d != "" {
			writeWrapped(w, strings.SplitN(d, "\n\n", 2)[0], "    ", 80)
		}
		_, _ = fmt.Fprintln(w)
	}
}

// tree lists every field below s, by name and type; seen keeps a message that
// holds itself from being listed forever.
func (root jsonSchema) tree(w io.Writer, s jsonSchema, indent string, seen map[string]bool) {
	props, _ := s.sub("properties")
	for _, name := range sortedKeys(props) {
		p, _ := props[name].(map[string]any)
		ps := jsonSchema(p)
		_, _ = fmt.Fprintf(w, "%s%s\t<%s>\n", indent, name, root.typeName(ps))
		ref := root.refOf(ps)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		root.tree(w, root.element(ps), indent+"  ", seen)
		delete(seen, ref)
	}
}

// refOf is the definition a field's elements are, if any.
func (root jsonSchema) refOf(s jsonSchema) string {
	for range 32 {
		if ref := s.str("$ref"); ref != "" {
			return ref
		}
		if items, ok := s.sub("items"); ok {
			s = items
			continue
		}
		if v, ok := s.sub("additionalProperties"); ok {
			s = v
			continue
		}
		return ""
	}
	return ""
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// writeWrapped writes text wrapped at width, each line indented; the text's
// own line breaks, between paragraphs and list items, are kept.
func writeWrapped(w io.Writer, text, indent string, width int) {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			_, _ = fmt.Fprintln(w)
			continue
		}
		cur := indent
		for _, word := range strings.Fields(line) {
			if len(cur) > len(indent) && len(cur)+1+len(word) > width {
				_, _ = fmt.Fprintln(w, cur)
				cur = indent + "  "
				if !strings.HasPrefix(line, "- ") {
					cur = indent
				}
			}
			if len(cur) > len(indent) && !strings.HasSuffix(cur, " ") {
				cur += " "
			}
			cur += word
		}
		_, _ = fmt.Fprintln(w, cur)
	}
}
